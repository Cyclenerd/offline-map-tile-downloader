// The main package is the entry point for the application.
package main

// Import necessary libraries.
import (
	"bytes"
	"context"
	"embed" // Used for embedding files into the binary.
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"io/fs"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket" // WebSocket library for real-time communication.
)

//go:embed templates/index.html
var indexHTML []byte // Embeds the index.html file into the binary.

//go:embed config/map_sources.json
var mapSourcesJSON []byte // Embeds the map_sources.json file into the binary.

//go:embed static/*
var staticFiles embed.FS

// upgrader is used to upgrade HTTP connections to WebSocket connections.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024, // Size of the read buffer.
	WriteBufferSize: 1024, // Size of the write buffer.
}

// Global variables used throughout the application.
var (
	mapSources       map[string]string  // Stores the available map sources.
	downloadCancel   context.CancelFunc // Function to cancel an ongoing download.
	downloading      bool               // Flag to indicate if a download is in progress.
	downloadingMutex sync.Mutex         // Mutex to protect access to the downloading flag.
	cacheDir         *string
	maxWorkers       *int
	rateLimit        *int
	maxRetries       *int
)

// Tile represents a single map tile with X, Y coordinates and zoom level Z.
type Tile struct {
	X, Y, Z uint32
}

// BoundingBox represents a geographical area with North, South, East, and West boundaries.
type BoundingBox struct {
	North, South, East, West float64
}

// LatLng represents a geographical point with latitude and longitude.
type LatLng struct {
	Lat float64 `json:"lat"` // Latitude
	Lng float64 `json:"lng"` // Longitude
}

// DownloadRequest represents a request to download map tiles for a specific area.
type DownloadRequest struct {
	Polygons      [][]LatLng `json:"polygons"`        // The polygons defining the download area.
	MinZoom       int        `json:"min_zoom"`        // The minimum zoom level to download.
	MaxZoom       int        `json:"max_zoom"`        // The maximum zoom level to download.
	MapStyle      string     `json:"map_style"`       // The URL of the map tile server.
	ConvertTo8Bit bool       `json:"convert_to_8bit"` // Whether to convert images to 8-bit PNG.
}

// WorldDownloadRequest represents a request to download map tiles for the entire world.
type WorldDownloadRequest struct {
	MapStyle      string `json:"map_style"`       // The URL of the map tile server.
	ConvertTo8Bit bool   `json:"convert_to_8bit"` // Whether to convert images to 8-bit PNG.
}

// WSMessage represents a WebSocket message with a type and data.
type WSMessage struct {
	Type string      `json:"type"` // The type of the message (e.g., "start_download").
	Data interface{} `json:"data"` // The data associated with the message.
}

// main is the entry point of the application.
func main() {
	// Command line flags
	port := flag.Int("port", 8080, "Port number for the server")
	cacheDir = flag.String("maps-directory", "maps", "Directory for storing map tiles. This is where the downloaded tiles will be saved.")
	maxWorkers = flag.Int("max-workers", 10, "Number of concurrent download workers")
	rateLimit = flag.Int("rate-limit", 50, "Maximum number of tiles to download per second")
	maxRetries = flag.Int("max-retries", 3, "Maximum number of retries for downloading a tile")
	help := flag.Bool("help", false, "Show help message")

	flag.Parse()

	if *help {
		flag.Usage()
		return
	}

	// Create cache directory if it doesn't exist.
	if err := os.MkdirAll(*cacheDir, 0755); err != nil {
		log.Fatalf("Failed to create cache directory: %v", err)
	}

	// Load map sources from the embedded JSON file.
	if err := json.Unmarshal(mapSourcesJSON, &mapSources); err != nil {
		log.Fatalf("Failed to load map sources: %v", err)
	}

	// Register HTTP handlers for different routes.
	http.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		r.URL.Path = "/static/favicon.ico"
		http.FileServer(http.FS(staticFiles)).ServeHTTP(w, r)
	})
	http.HandleFunc("/", serveHome)
	http.HandleFunc("/get_map_sources", getMapSources)
	http.HandleFunc("/ws", wsHandler)

	http.HandleFunc("/tiles/", serveTile)
	http.HandleFunc("/get_cached_tiles/", getCachedTiles)

	staticFS, err := fs.Sub(staticFiles, "static")
	if err != nil {
		log.Fatal(err)
	}
	http.Handle("/static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

	// Start the HTTP server on port 8080.
	addr := fmt.Sprintf(":%d", *port)
	log.Printf("Starting server on %s", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		log.Fatalf("Error starting server: %v", err)
	}
}

// serveHome serves the main HTML page.
func serveHome(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html")
	if _, err := w.Write(indexHTML); err != nil {
		log.Printf("Could not write response: %v", err)
	}
}

// getMapSources serves the available map sources as JSON.
func getMapSources(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(mapSourcesJSON); err != nil {
		log.Printf("Could not write response: %v", err)
	}
}

// wsHandler handles WebSocket connections.
func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket connection.
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println(err)
		return
	}
	defer func() {
		if err := conn.Close(); err != nil {
			log.Printf("Could not close websocket connection: %v", err)
		}
	}()

	// Loop to read messages from the WebSocket connection.
	for {
		messageType, p, err := conn.ReadMessage()
		if err != nil {
			log.Println(err)
			return
		}
		if messageType == websocket.TextMessage {
			var msg WSMessage
			if err := json.Unmarshal(p, &msg); err != nil {
				log.Println("Error unmarshalling message:", err)
				continue
			}

			// Handle different message types.
			switch msg.Type {
			case "start_download":
				var req DownloadRequest
				b, _ := json.Marshal(msg.Data)
				if err := json.Unmarshal(b, &req); err != nil {
					sendError(conn, "Invalid download request")
					continue
				}
				go handleStartDownload(conn, req)
			case "start_world_download":
				var req WorldDownloadRequest
				b, _ := json.Marshal(msg.Data)
				if err := json.Unmarshal(b, &req); err != nil {
					sendError(conn, "Invalid world download request")
					continue
				}
				go handleStartWorldDownload(conn, req)
			case "cancel_download":
				handleCancelDownload(conn)
			}
		}
	}
}

// handleStartDownload starts a new download process for a defined area.
func handleStartDownload(conn *websocket.Conn, req DownloadRequest) {
	// Lock the mutex to ensure only one download runs at a time.
	downloadingMutex.Lock()
	if downloading {
		sendError(conn, "Another download is already in progress.")
		downloadingMutex.Unlock()
		return
	}
	downloading = true
	downloadingMutex.Unlock()

	// Defer setting the downloading flag to false.
	defer func() {
		downloadingMutex.Lock()
		downloading = false
		downloadingMutex.Unlock()
	}()

	log.Printf("Starting download for area: %v, zoom: %d-%d, map style: %s", req.Polygons, req.MinZoom, req.MaxZoom, req.MapStyle)

	// Create a new context to allow for cancellation.
	var ctx context.Context
	ctx, downloadCancel = context.WithCancel(context.Background())

	// Get the style name and cache directory.
	styleName := getStyleName(req.MapStyle)
	styleCacheDir := getStyleCacheDir(styleName)

	// Validate the zoom range.
	if req.MinZoom < 0 || req.MaxZoom > 19 || req.MinZoom > req.MaxZoom {
		sendError(conn, "Invalid zoom range (must be 0-19, min <= max)")
		return
	}
	// Validate the polygons.
	if len(req.Polygons) == 0 {
		sendError(conn, "No polygons provided")
		return
	}

	// Get the list of tiles to download.
	tilesToDownload := getTilesForPolygons(req.Polygons, req.MinZoom, req.MaxZoom)

	// Start the tile download process.
	downloadTiles(ctx, conn, tilesToDownload, req.MapStyle, styleCacheDir, req.ConvertTo8Bit)

	// If the download was not cancelled
	if ctx.Err() == nil {
		sendMessage(conn, "download_complete", nil)
	}
}

// handleStartWorldDownload starts a new download process for the entire world.
func handleStartWorldDownload(conn *websocket.Conn, req WorldDownloadRequest) {
	// Lock the mutex to ensure only one download runs at a time.
	downloadingMutex.Lock()
	if downloading {
		sendError(conn, "Another download is already in progress.")
		downloadingMutex.Unlock()
		return
	}
	downloading = true
	downloadingMutex.Unlock()

	// Defer setting the downloading flag to false.
	defer func() {
		downloadingMutex.Lock()
		downloading = false
		downloadingMutex.Unlock()
	}()

	log.Printf("Starting world download, map style: %s", req.MapStyle)

	// Create a new context to allow for cancellation.
	var ctx context.Context
	ctx, downloadCancel = context.WithCancel(context.Background())

	// Get the style name and cache directory.
	styleName := getStyleName(req.MapStyle)
	styleCacheDir := getStyleCacheDir(styleName)

	// Get the list of tiles to download for the world.
	tilesToDownload := getWorldTiles()

	// Start the tile download process.
	downloadTiles(ctx, conn, tilesToDownload, req.MapStyle, styleCacheDir, req.ConvertTo8Bit)

	// If the download was not cancelled
	if ctx.Err() == nil {
		sendMessage(conn, "download_complete", nil)
	}
}

// handleCancelDownload cancels an ongoing download.
func handleCancelDownload(conn *websocket.Conn) {
	if downloadCancel != nil {
		downloadCancel()
		log.Printf("Download cancelled by user")
		sendMessage(conn, "download_cancelled", nil)
	}
}

// downloadTiles downloads a list of tiles concurrently.
func downloadTiles(ctx context.Context, conn *websocket.Conn, tilesToDownload []Tile, mapStyle, styleCacheDir string, convertTo8Bit bool) {
	// Create a channel for WebSocket messages.
	msgChan := make(chan WSMessage)
	var writerWg sync.WaitGroup
	writerWg.Add(1)
	// Start a goroutine to send messages from the channel to the WebSocket connection.
	go func() {
		defer writerWg.Done()
		for msg := range msgChan {
			if err := conn.WriteJSON(msg); err != nil {
				log.Println("Error writing JSON to websocket:", err)
				return
			}
		}
	}()

	// Send a message indicating the download has started.
	msgChan <- WSMessage{Type: "download_started", Data: map[string]int{"total_tiles": len(tilesToDownload)}}

	// Use a WaitGroup to wait for all download goroutines to finish.
	var downloadWg sync.WaitGroup
	tileChan := make(chan Tile)

	// Start the download workers.
	for i := 0; i < *maxWorkers; i++ {
		downloadWg.Add(1)
		go func() {
			defer downloadWg.Done()
			for tile := range tileChan {
				select {
				case <-ctx.Done(): // Check if the download has been cancelled.
					return
				default:
					downloadTile(ctx, msgChan, tile, mapStyle, styleCacheDir, convertTo8Bit, *maxRetries)
				}
			}
		}()
	}

	// Rate limit the download of tiles.
	ticker := time.NewTicker(time.Second / time.Duration(*rateLimit))
	defer ticker.Stop()

	DownloadLoop:
	for _, tile := range tilesToDownload {
		select {
		case <-ctx.Done():
			break DownloadLoop
		case <-ticker.C:
			tileChan <- tile
		}
	}
	close(tileChan)

	// Wait for all downloads to complete.
	downloadWg.Wait()
	close(msgChan)
	writerWg.Wait()

	// If the download was not cancelled, send a completion message.
	if ctx.Err() == nil {
		log.Printf("Download finished successfully")
		sendMessage(conn, "tiles_downloaded", nil)
	} else {
		log.Printf("Download failed or was cancelled")
	}
}

// downloadTile downloads a single map tile.
func downloadTile(ctx context.Context, msgChan chan<- WSMessage, tile Tile, mapStyle, styleCacheDir string, convertTo8Bit bool, maxRetries int) {
	// Construct the path to the tile file.
	tileDir := filepath.Join(styleCacheDir, fmt.Sprintf("%d/%d", tile.Z, tile.X))
	tilePath := filepath.Join(tileDir, fmt.Sprintf("%d.png", tile.Y))

	// Check if the tile already exists in the cache.
	if _, err := os.Stat(tilePath); err == nil {
		bounds := tileBounds(tile)
		msgChan <- WSMessage{Type: "tile_skipped", Data: map[string]float64{
			"west":  bounds.West,
			"south": bounds.South,
			"east":  bounds.East,
			"north": bounds.North,
		}}
		return
	}

	// Construct the URL for the tile.
	subdomain := []string{"a", "b", "c"}[rand.Intn(3)]
	url := strings.ReplaceAll(mapStyle, "{s}", subdomain)
	url = strings.ReplaceAll(url, "{z}", fmt.Sprintf("%d", tile.Z))
	url = strings.ReplaceAll(url, "{x}", fmt.Sprintf("%d", tile.X))
	url = strings.ReplaceAll(url, "{y}", fmt.Sprintf("%d", tile.Y))

	var err error
	for attempt := 0; attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done(): // Check for cancellation.
			return
		default:
		}

		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			log.Printf("Error creating request for tile %v: %v. Retrying...", tile, err)
			time.Sleep(time.Second * time.Duration(math.Pow(2, float64(attempt))))
			continue
		}
		req.Header.Set("User-Agent", "MapTileDownloader/1.0 (Go)")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			log.Printf("Error downloading tile %v: %v. Retrying...", tile, err)
			time.Sleep(time.Second * time.Duration(math.Pow(2, float64(attempt))))
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if err := resp.Body.Close(); err != nil {
				log.Printf("Could not close response body: %v", err)
			}
			log.Printf("Unexpected status code %d for tile %v. Retrying...", resp.StatusCode, tile)
			time.Sleep(time.Second * time.Duration(math.Pow(2, float64(attempt))))
			continue
		}

		body, err := io.ReadAll(resp.Body)
		if err := resp.Body.Close(); err != nil {
			log.Printf("Could not close response body: %v", err)
		}
		if err != nil {
			log.Printf("Error reading tile body for tile %v: %v. Retrying...", tile, err)
			time.Sleep(time.Second * time.Duration(math.Pow(2, float64(attempt))))
			continue
		}

		if err := os.MkdirAll(tileDir, 0755); err != nil {
			log.Printf("Error creating tile directory for tile %v: %v", tile, err)
			return // No point in retrying if we can't create the directory
		}

		// Convert the image to 8-bit PNG if requested.
		if convertTo8Bit {
			img, _, err := image.Decode(bytes.NewReader(body))
			if err == nil {
				paletted := image.NewPaletted(img.Bounds(), color.Palette{})
				draw.Draw(paletted, paletted.Rect, img, img.Bounds().Min, draw.Src)
				var buf bytes.Buffer
				if err := png.Encode(&buf, paletted); err == nil {
					body = buf.Bytes()
				}
			}
		}

		if err := os.WriteFile(tilePath, body, 0644); err != nil {
			log.Printf("Error writing tile %v: %v", tile, err)
			return // No point in retrying if we can't write the file
		}

		bounds := tileBounds(tile)
		msgChan <- WSMessage{Type: "tile_downloaded", Data: map[string]float64{
			"west":  bounds.West,
			"south": bounds.South,
			"east":  bounds.East,
			"north": bounds.North,
		}}
		return // Success!
	}

	// If all retries fail, send a failure message.
	log.Printf("Failed to download tile %v after %d attempts.", tile, maxRetries)
	msgChan <- WSMessage{Type: "tile_failed", Data: map[string]string{"tile": fmt.Sprintf("%d/%d/%d", tile.Z, tile.X, tile.Y)}}
}

// getTilesForPolygons calculates the tiles needed to cover the given polygons.
func getTilesForPolygons(polygonsData [][]LatLng, minZoom, maxZoom int) []Tile {
	var allTiles []Tile
	tileMap := make(map[Tile]bool)

	for _, polyData := range polygonsData {
		if len(polyData) < 3 {
			continue
		}

		minLat, minLon := 90.0, 180.0
		maxLat, maxLon := -90.0, -180.0
		for _, p := range polyData {
			if p.Lat < minLat {
				minLat = p.Lat
			}
			if p.Lat > maxLat {
				maxLat = p.Lat
			}
			if p.Lng < minLon {
				minLon = p.Lng
			}
			if p.Lng > maxLon {
				maxLon = p.Lng
			}
		}

		for z := minZoom; z <= maxZoom; z++ {
			tlx, tly := latLonToTile(maxLat, minLon, uint32(z))
			brx, bry := latLonToTile(minLat, maxLon, uint32(z))

			for x := tlx; x <= brx; x++ {
				for y := tly; y <= bry; y++ {
					tile := Tile{X: x, Y: y, Z: uint32(z)}
					if _, exists := tileMap[tile]; exists {
						continue
					}

					bounds := tileBounds(tile)

					// Check if the tile is completely inside the polygon
					if polygonContains(polyData, LatLng{Lat: bounds.North, Lng: bounds.West}) &&
						polygonContains(polyData, LatLng{Lat: bounds.North, Lng: bounds.East}) &&
						polygonContains(polyData, LatLng{Lat: bounds.South, Lng: bounds.West}) &&
						polygonContains(polyData, LatLng{Lat: bounds.South, Lng: bounds.East}) {
						allTiles = append(allTiles, tile)
						tileMap[tile] = true
						continue
					}

					// Check if the polygon is completely inside the tile
					polyInTile := true
					for _, p := range polyData {
						if !tileContains(bounds, p) {
							polyInTile = false
							break
						}
					}
					if polyInTile {
						allTiles = append(allTiles, tile)
						tileMap[tile] = true
						continue
					}

					// Check for intersection
					if polygonIntersects(polyData, bounds) {
						allTiles = append(allTiles, tile)
						tileMap[tile] = true
					}
				}
			}
		}
	}

	return allTiles
}

// tileContains checks if a tile contains a point.
func tileContains(bounds BoundingBox, point LatLng) bool {
	return point.Lat <= bounds.North && point.Lat >= bounds.South && point.Lng >= bounds.West && point.Lng <= bounds.East
}

// polygonIntersects checks if a polygon intersects with a tile.
func polygonIntersects(poly []LatLng, bounds BoundingBox) bool {
	// Check if any of the polygon's vertices are inside the tile
	for _, p := range poly {
		if tileContains(bounds, p) {
			return true
		}
	}

	// Check if any of the tile's corners are inside the polygon
	if polygonContains(poly, LatLng{Lat: bounds.North, Lng: bounds.West}) ||
		polygonContains(poly, LatLng{Lat: bounds.North, Lng: bounds.East}) ||
		polygonContains(poly, LatLng{Lat: bounds.South, Lng: bounds.West}) ||
		polygonContains(poly, LatLng{Lat: bounds.South, Lng: bounds.East}) {
		return true
	}

	// Check if any of the polygon's edges intersect with the tile's edges
	for i := 0; i < len(poly); i++ {
		p1 := poly[i]
		p2 := poly[(i+1)%len(poly)]

		if lineIntersects(p1, p2, LatLng{Lat: bounds.North, Lng: bounds.West}, LatLng{Lat: bounds.North, Lng: bounds.East}) ||
			lineIntersects(p1, p2, LatLng{Lat: bounds.North, Lng: bounds.East}, LatLng{Lat: bounds.South, Lng: bounds.East}) ||
			lineIntersects(p1, p2, LatLng{Lat: bounds.South, Lng: bounds.East}, LatLng{Lat: bounds.South, Lng: bounds.West}) ||
			lineIntersects(p1, p2, LatLng{Lat: bounds.South, Lng: bounds.West}, LatLng{Lat: bounds.North, Lng: bounds.West}) {
			return true
		}
	}

	return false
}

// lineIntersects checks if two line segments intersect.
func lineIntersects(p1, q1, p2, q2 LatLng) bool {
	o1 := orientation(p1, q1, p2)
	o2 := orientation(p1, q1, q2)
	o3 := orientation(p2, q2, p1)
	o4 := orientation(p2, q2, q1)

	if o1 != o2 && o3 != o4 {
		return true
	}

	// Special Cases for colinear points
	if o1 == 0 && onSegment(p1, p2, q1) {
		return true
	}
	if o2 == 0 && onSegment(p1, q2, q1) {
		return true
	}
	if o3 == 0 && onSegment(p2, p1, q2) {
		return true
	}
	if o4 == 0 && onSegment(p2, q1, q2) {
		return true
	}

	return false
}

// orientation finds the orientation of the ordered triplet (p, q, r).
func orientation(p, q, r LatLng) int {
	val := (q.Lng-p.Lng)*(r.Lat-q.Lat) - (q.Lat-p.Lat)*(r.Lng-q.Lng)
	if val == 0 {
		return 0 // Collinear
	}
	if val > 0 {
		return 1 // Clockwise
	}
	return 2 // Counterclockwise
}

// onSegment checks if point q lies on segment pr.
func onSegment(p, q, r LatLng) bool {
	if q.Lat <= math.Max(p.Lat, r.Lat) && q.Lat >= math.Min(p.Lat, r.Lat) &&
		q.Lng <= math.Max(p.Lng, r.Lng) && q.Lng >= math.Min(p.Lng, r.Lng) {
		return true
	}
	return false
}

// getWorldTiles returns a list of all tiles for the world up to zoom level 7.
func getWorldTiles() []Tile {
	var worldTiles []Tile
	for z := 0; z <= 7; z++ {
		max := 1 << z
		for x := 0; x < max; x++ {
			for y := 0; y < max; y++ {
				worldTiles = append(worldTiles, Tile{X: uint32(x), Y: uint32(y), Z: uint32(z)})
			}
		}
	}
	return worldTiles
}

// serveTile serves a single cached tile.
func serveTile(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/tiles/"), "/")
	if len(parts) != 4 {
		http.NotFound(w, r)
		return
	}
	styleName := parts[0]
	z := parts[1]
	x := parts[2]
	y := strings.TrimSuffix(parts[3], ".png")

	tilePath := filepath.Join(*cacheDir, sanitizeStyleName(styleName), z, x, y+".png")
	http.ServeFile(w, r, tilePath)
}

// getCachedTiles returns a list of cached tiles for a specific map style.
func getCachedTiles(w http.ResponseWriter, r *http.Request) {
	styleName := strings.TrimPrefix(r.URL.Path, "/get_cached_tiles/")
	styleCacheDir := getStyleCacheDir(styleName)

	var cachedTiles [][3]uint32
	err := filepath.Walk(styleCacheDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(info.Name(), ".png") {
			parts := strings.Split(strings.TrimSuffix(path, ".png"), string(filepath.Separator))
			if len(parts) >= 4 {
				z, zErr := strToUint32(parts[len(parts)-3])
				x, xErr := strToUint32(parts[len(parts)-2])
				y, yErr := strToUint32(parts[len(parts)-1])
				if zErr == nil && xErr == nil && yErr == nil {
					cachedTiles = append(cachedTiles, [3]uint32{z, x, y})
				}
			}
		}
		return nil
	})

	if err != nil {
		http.Error(w, fmt.Sprintf("Error reading cache: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(cachedTiles); err != nil {
		http.Error(w, fmt.Sprintf("Error encoding cached tiles: %v", err), http.StatusInternalServerError)
	}
}

// getStyleName returns the name of the map style for a given URL.
func getStyleName(mapStyleURL string) string {
	for name, url := range mapSources {
		if url == mapStyleURL {
			return name
		}
	}
	return "default"
}

// getStyleCacheDir returns the cache directory for a given style name.
func getStyleCacheDir(styleName string) string {
	return filepath.Join(*cacheDir, sanitizeStyleName(styleName))
}

// nonAlphanumeric is a regular expression to match any character that is not a letter, number, hyphen, or underscore.
var nonAlphanumeric = regexp.MustCompile(`[^a-zA-Z0-9-_]+`)

// sanitizeStyleName sanitizes the style name to be used as a directory name.
func sanitizeStyleName(styleName string) string {
	return nonAlphanumeric.ReplaceAllString(strings.ReplaceAll(styleName, " ", "-"), "")
}

// sendMessage sends a WebSocket message.
func sendMessage(conn *websocket.Conn, msgType string, data interface{}) {
	msg := WSMessage{Type: msgType, Data: data}
	if err := conn.WriteJSON(msg); err != nil {
		log.Println("Error sending message:", err)
	}
}

// sendError sends an error message over the WebSocket connection.
func sendError(conn *websocket.Conn, message string) {
	sendMessage(conn, "error", map[string]string{"message": message})
}

// strToUint32 converts a string to a uint32.
func strToUint32(s string) (uint32, error) {
	var i uint32
	_, err := fmt.Sscanf(s, "%d", &i)
	return i, err
}

// latLonToTile converts latitude and longitude to tile coordinates.
func latLonToTile(lat, lon float64, zoom uint32) (x, y uint32) {
	latRad := lat * math.Pi / 180
	n := math.Pow(2, float64(zoom))
	x = uint32(n * ((lon + 180) / 360))
	y = uint32(n * (1 - (math.Log(math.Tan(latRad)+1/math.Cos(latRad)) / math.Pi)) / 2)
	return
}

// tileBounds calculates the geographical bounding box of a tile.
func tileBounds(tile Tile) BoundingBox {
	n := math.Pow(2.0, float64(tile.Z))
	lonDeg := float64(tile.X)/n*360.0 - 180.0
	latRad := math.Atan(math.Sinh(math.Pi * (1 - 2*float64(tile.Y)/n)))
	latDeg := latRad * 180.0 / math.Pi

	lon2Deg := float64(tile.X+1)/n*360.0 - 180.0
	lat2Rad := math.Atan(math.Sinh(math.Pi * (1 - 2*float64(tile.Y+1)/n)))
	lat2Deg := lat2Rad * 180.0 / math.Pi

	return BoundingBox{
		North: latDeg,
		South: lat2Deg,
		East:  lon2Deg,
		West:  lonDeg,
	}
}

// polygonContains checks if a point is inside a polygon using the ray casting algorithm.
func polygonContains(poly []LatLng, point LatLng) bool {
	in := false
	for i, j := 0, len(poly)-1; i < len(poly); j, i = i, i+1 {
		if (poly[i].Lat > point.Lat) != (poly[j].Lat > point.Lat) &&
			(point.Lng < (poly[j].Lng-poly[i].Lng)*(point.Lat-poly[i].Lat)/(poly[j].Lat-poly[i].Lat)+poly[i].Lng) {
			in = !in
		}
	}
	return in
}
