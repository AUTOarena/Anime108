package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

type DownloadStatus struct {
	Status     string `json:"status"`
	Progress   int    `json:"progress"`
	Total      int    `json:"total"`
	Percentage int    `json:"percentage"`
	Message    string `json:"message"`
	Title      string `json:"title"`
	Lang       string `json:"lang"`
	URL        string `json:"url"`
}

type Server struct {
	templates   *template.Template
	downloadDir string
	mu          sync.RWMutex
	downloads   map[string]DownloadStatus
}

func NewServer(downloadDir string) (*Server, error) {
	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(downloadDir, 0o755); err != nil {
		return nil, err
	}
	return &Server{templates: templates, downloadDir: downloadDir, downloads: make(map[string]DownloadStatus)}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

type RequestPayload struct {
	URL  string `json:"url"`
	Lang string `json:"lang"`
}

func decodeRequest(w http.ResponseWriter, r *http.Request) (RequestPayload, bool) {
	var payload RequestPayload
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid JSON body: %w", err))
		return payload, false
	}
	payload.URL = strings.TrimSpace(payload.URL)
	if payload.URL == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("URL is required"))
		return payload, false
	}
	if payload.Lang == "" {
		payload.Lang = "Sound Track"
	}
	if payload.Lang != "Sound Track" && payload.Lang != "Thai" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("lang must be Sound Track or Thai"))
		return payload, false
	}
	return payload, true
}

func method(method string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
			return
		}
		handler(w, r)
	}
}

func (s *Server) render(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && name == "index.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.templates.ExecuteTemplate(w, name, nil); err != nil {
			log.Printf("render %s: %v", name, err)
		}
	}
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	scraper := NewScraper()
	document, err := scraper.GetPageContent(r.Context(), payload.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	metadata, err := scraper.ParseShowPage(document, payload.URL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, metadata)
}

func (s *Server) playerURL(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	scraper := NewScraper()
	document, err := scraper.GetPageContent(r.Context(), payload.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	metadata, err := scraper.ParseShowPage(document, payload.URL)
	if err == nil && metadata.PostID == 0 {
		err = fmt.Errorf("could not find post ID on the page")
	}
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	iframe, err := scraper.GetPlayerIframe(r.Context(), metadata.PostID, metadata.Episode, metadata.Server, payload.Lang, metadata.Title)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"iframe_url": iframe})
}

func taskID() (string, error) {
	data := make([]byte, 16)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	data[6] = (data[6] & 0x0f) | 0x40
	data[8] = (data[8] & 0x3f) | 0x80
	value := hex.EncodeToString(data)
	return value[:8] + "-" + value[8:12] + "-" + value[12:16] + "-" + value[16:20] + "-" + value[20:], nil
}

func (s *Server) updateTask(id string, update func(*DownloadStatus)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := s.downloads[id]
	update(&status)
	s.downloads[id] = status
}

func (s *Server) runDownload(id, target, lang string) {
	s.updateTask(id, func(status *DownloadStatus) {
		status.Status = "downloading"
		status.Message = "Initiating connections and resolving streaming sources..."
	})
	scraper := NewScraper()
	document, err := scraper.GetPageContent(context.Background(), target)
	if err == nil {
		var metadata Metadata
		metadata, err = scraper.ParseShowPage(document, target)
		if err == nil {
			s.updateTask(id, func(status *DownloadStatus) {
				status.Title = fmt.Sprintf("%s - Ep %d", metadata.Title, metadata.Episode)
			})
		}
	}
	if err == nil {
		_, err = scraper.DownloadVideo(context.Background(), target, s.downloadDir, lang, 16, false, func(state string, current, total int, message string) {
			s.updateTask(id, func(status *DownloadStatus) {
				status.Status, status.Progress, status.Total, status.Message = state, current, total, message
				if total > 0 {
					status.Percentage = current * 100 / total
				}
			})
		})
	}
	if err != nil {
		log.Printf("download %s failed: %v", id, err)
		s.updateTask(id, func(status *DownloadStatus) {
			status.Status = "failed"
			status.Message = "Error: " + err.Error()
		})
		return
	}
	s.updateTask(id, func(status *DownloadStatus) {
		status.Status = "completed"
		status.Percentage = 100
		if !strings.HasPrefix(status.Message, "Saved final MP4") {
			status.Message = "Download completed successfully"
		}
	})
}

func (s *Server) download(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	id, err := taskID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.mu.Lock()
	s.downloads[id] = DownloadStatus{Status: "idle", Message: "Waiting to start...", Title: "Fetching show info...", Lang: payload.Lang, URL: payload.URL}
	s.mu.Unlock()
	go s.runDownload(id, payload.URL, payload.Lang)
	writeJSON(w, http.StatusOK, map[string]string{"task_id": id})
}

func (s *Server) progress(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/progress/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}
	s.mu.RLock()
	status, found := s.downloads[id]
	s.mu.RUnlock()
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("task not found"))
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) listDownloads(w http.ResponseWriter, r *http.Request) {
	entries, err := os.ReadDir(s.downloadDir)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	type item struct {
		Filename string `json:"filename"`
		Size     string `json:"size"`
		Path     string `json:"path"`
	}
	files := []item{}
	for _, entry := range entries {
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".mp4") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files = append(files, item{entry.Name(), fmt.Sprintf("%.1f MB", float64(info.Size())/(1024*1024)), filepath.Join(s.downloadDir, entry.Name())})
	}
	writeJSON(w, http.StatusOK, map[string]any{"downloads": files})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	if keyword == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf(`query parameter "keyword" or "q" is required`))
		return
	}
	results, err := NewScraper().SearchAnime(r.Context(), keyword)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", method(http.MethodGet, s.render("index.html")))
	mux.HandleFunc("/docs", method(http.MethodGet, s.render("docs.html")))
	mux.HandleFunc("/search", method(http.MethodGet, s.search))
	mux.HandleFunc("/api/parse", method(http.MethodPost, s.parse))
	mux.HandleFunc("/api/player-url", method(http.MethodPost, s.playerURL))
	mux.HandleFunc("/api/download", method(http.MethodPost, s.download))
	mux.HandleFunc("/api/progress/", method(http.MethodGet, s.progress))
	mux.HandleFunc("/api/downloads", method(http.MethodGet, s.listDownloads))
	return mux
}

func main() {
	pageURL := flag.String("url", "", "Anime108 show or episode URL (runs the downloader CLI)")
	outputDir := flag.String("dir", "downloads", "directory for downloaded videos")
	lang := flag.String("lang", "Sound Track", "language: Sound Track or Thai")
	threads := flag.Int("threads", 16, "number of concurrent segment downloads")
	checkOnly := flag.Bool("check-only", false, "resolve the playlist without downloading")
	port := flag.Int("port", 5000, "HTTP server port")
	flag.Parse()

	if *lang != "Sound Track" && *lang != "Thai" {
		log.Fatal("-lang must be Sound Track or Thai")
	}
	if *pageURL != "" {
		result, err := NewScraper().DownloadVideo(context.Background(), *pageURL, *outputDir, *lang, *threads, *checkOnly, func(status string, current, total int, message string) {
			fmt.Printf("[%s] %s (%d/%d)\n", status, message, current, total)
		})
		if err != nil {
			log.Fatal(err)
		}
		if result.StreamURL != "" {
			fmt.Println("Stream URL:", result.StreamURL)
		} else {
			fmt.Println("Saved:", result.Filepath)
		}
		return
	}

	absoluteDir, err := filepath.Abs(*outputDir)
	if err != nil {
		log.Fatal(err)
	}
	server, err := NewServer(absoluteDir)
	if err != nil {
		log.Fatal(err)
	}
	address := ":" + strconv.Itoa(*port)
	log.Printf("Starting Anime108 Go server at http://localhost:%d", *port)
	log.Printf("Videos will be downloaded to: %s", absoluteDir)
	log.Fatal(http.ListenAndServe(address, server.Handler()))
}
