package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type RequestPayload struct {
	URL  string `json:"url"`
	Lang string `json:"lang"`
}

type Server struct {
	templates *template.Template
	proxy     *HLSProxy
}

func NewServer() (*Server, error) {
	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		return nil, err
	}
	return &Server{templates: templates, proxy: NewHLSProxy(2 * time.Hour)}, nil
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
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
	parsed, err := url.Parse(payload.URL)
	if err != nil || parsed.Scheme != "https" || (parsed.Hostname() != "anime108.com" && parsed.Hostname() != "www.anime108.com") {
		writeError(w, http.StatusBadRequest, fmt.Errorf("URL must be an HTTPS anime108.com URL"))
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

func requireMethod(expected string, handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != expected {
			w.Header().Set("Allow", expected)
			writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
			return
		}
		handler(w, r)
	}
}

func (s *Server) render(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if name == "index.html" && r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := s.templates.ExecuteTemplate(w, name, nil); err != nil {
			log.Printf("render %s: %v", name, err)
		}
	}
}

func loadMetadata(r *http.Request, target string) (*Scraper, Metadata, error) {
	scraper := NewScraper()
	document, err := scraper.GetPageContent(r.Context(), target)
	if err != nil {
		return scraper, Metadata{}, err
	}
	metadata, err := scraper.ParseShowPage(document, target)
	return scraper, metadata, err
}

func (s *Server) parse(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	_, metadata, err := loadMetadata(r, payload.URL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, metadata)
}

func (s *Server) resolveStream(w http.ResponseWriter, r *http.Request) {
	payload, ok := decodeRequest(w, r)
	if !ok {
		return
	}
	scraper, metadata, err := loadMetadata(r, payload.URL)
	if err == nil && metadata.PostID == 0 {
		err = fmt.Errorf("could not find post ID on the page")
	}
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	iframeURL, err := scraper.GetPlayerIframe(r.Context(), metadata.PostID, metadata.Episode, metadata.Server, payload.Lang, metadata.Title)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	streamURL, err := scraper.ResolveStreamURL(r.Context(), iframeURL)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	token, err := s.proxy.Register(streamURL, iframeURL)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"playlist_url": "/hls/" + token,
		"title":        metadata.Title,
		"episode":      metadata.Episode,
		"lang":         payload.Lang,
		"expires_in":   int(s.proxy.ttl.Seconds()),
	})
}

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	keyword := strings.TrimSpace(r.URL.Query().Get("keyword"))
	if keyword == "" {
		keyword = strings.TrimSpace(r.URL.Query().Get("q"))
	}
	if keyword == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("query parameter \"keyword\" or \"q\" is required"))
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
	mux.HandleFunc("/", requireMethod(http.MethodGet, s.render("index.html")))
	mux.HandleFunc("/docs", requireMethod(http.MethodGet, s.render("docs.html")))
	mux.HandleFunc("/search", requireMethod(http.MethodGet, s.search))
	mux.HandleFunc("/api/parse", requireMethod(http.MethodPost, s.parse))
	mux.HandleFunc("/api/stream", requireMethod(http.MethodPost, s.resolveStream))
	mux.Handle("/hls/", s.proxy)
	return mux
}

func main() {
	port := flag.Int("port", 5000, "HTTP server port")
	flag.Parse()
	server, err := NewServer()
	if err != nil {
		log.Fatal(err)
	}
	address := fmt.Sprintf(":%d", *port)
	log.Printf("Starting Anime108 HLS proxy at http://localhost:%d", *port)
	if err := http.ListenAndServe(address, server.Handler()); err != nil {
		log.Fatal(err)
	}
}
