package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

type proxyResource struct {
	URL       string
	Referer   string
	ExpiresAt time.Time
}

type HLSProxy struct {
	client    *http.Client
	ttl       time.Duration
	mu        sync.RWMutex
	resources map[string]proxyResource
	byTarget  map[string]string
	// variants maps "{master token}/{quality}" to the opaque token for that
	// variant playlist. It gives clients stable, readable quality URLs without
	// exposing an upstream URL.
	variants map[string]string
}

func NewHLSProxy(ttl time.Duration) *HLSProxy {
	return &HLSProxy{
		client:    &http.Client{Timeout: 90 * time.Second},
		ttl:       ttl,
		resources: make(map[string]proxyResource),
		byTarget:  make(map[string]string),
		variants:  make(map[string]string),
	}
}

func randomToken() (string, error) {
	data := make([]byte, 18)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func validUpstream(rawURL string) bool {
	parsed, err := url.Parse(rawURL)
	return err == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && parsed.Host != ""
}

func (p *HLSProxy) cleanupLocked(now time.Time) {
	for key, resource := range p.resources {
		if now.After(resource.ExpiresAt) {
			delete(p.resources, key)
			delete(p.byTarget, resource.URL+"\x00"+resource.Referer)
		}
	}
	for key, token := range p.variants {
		if _, found := p.resources[token]; !found {
			delete(p.variants, key)
		}
	}
}

func (p *HLSProxy) Register(rawURL, referer string) (string, error) {
	if !validUpstream(rawURL) {
		return "", fmt.Errorf("invalid HLS upstream URL")
	}
	targetKey := rawURL + "\x00" + referer
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cleanupLocked(now)
	if token, found := p.byTarget[targetKey]; found {
		resource := p.resources[token]
		resource.ExpiresAt = now.Add(p.ttl)
		p.resources[token] = resource
		return token, nil
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	p.resources[token] = proxyResource{URL: rawURL, Referer: referer, ExpiresAt: now.Add(p.ttl)}
	p.byTarget[targetKey] = token
	return token, nil
}

func (p *HLSProxy) resource(token string) (proxyResource, bool) {
	p.mu.RLock()
	resource, found := p.resources[token]
	p.mu.RUnlock()
	if !found || time.Now().After(resource.ExpiresAt) {
		if found {
			p.mu.Lock()
			delete(p.resources, token)
			delete(p.byTarget, resource.URL+"\x00"+resource.Referer)
			p.cleanupLocked(time.Now())
			p.mu.Unlock()
		}
		return proxyResource{}, false
	}
	return resource, true
}

var (
	playlistURI = regexp.MustCompile(`URI=("[^"]+"|'[^']+')`)
	resolution  = regexp.MustCompile(`(?i)(?:^|,)RESOLUTION=\d+x(\d+)(?:,|$)`)
	bandwidth   = regexp.MustCompile(`(?i)(?:^|,)BANDWIDTH=(\d+)(?:,|$)`)
)

func (p *HLSProxy) proxyURL(rawURL, referer string) (string, error) {
	token, err := p.Register(rawURL, referer)
	if err != nil {
		return "", err
	}
	return "/hls/" + token, nil
}

func resolveReference(base *url.URL, value string) (string, error) {
	reference, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

func qualityName(streamInfo string) string {
	if match := resolution.FindStringSubmatch(streamInfo); len(match) == 2 {
		return match[1] + "p"
	}
	if match := bandwidth.FindStringSubmatch(streamInfo); len(match) == 2 {
		// BANDWIDTH is bits per second. Keep a useful deterministic label for
		// master playlists which do not advertise RESOLUTION.
		var bps int64
		_, _ = fmt.Sscan(match[1], &bps)
		return fmt.Sprintf("%dkbps", bps/1000)
	}
	return "auto"
}

func (p *HLSProxy) variantURL(sessionID, quality, rawURL, referer string) (string, error) {
	token, err := p.Register(rawURL, referer)
	if err != nil {
		return "", err
	}
	key := sessionID + "/" + quality
	p.mu.Lock()
	p.variants[key] = token
	p.mu.Unlock()
	return "/hls/" + sessionID + "/" + quality + "/index.m3u8", nil
}

// rewritePlaylist is retained as a small compatibility wrapper for callers
// which do not have a master session ID (including media-playlist tests).
func (p *HLSProxy) rewritePlaylist(content []byte, sourceURL, referer string) ([]byte, error) {
	return p.rewritePlaylistForSession(content, sourceURL, referer, "")
}

func (p *HLSProxy) rewritePlaylistForSession(content []byte, sourceURL, referer, sessionID string) ([]byte, error) {
	base, err := url.Parse(sourceURL)
	if err != nil {
		return nil, err
	}
	var output strings.Builder
	var streamInfo string
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#EXT-X-STREAM-INF:") {
			streamInfo = strings.TrimPrefix(trimmed, "#EXT-X-STREAM-INF:")
		} else if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			resolved, resolveErr := resolveReference(base, trimmed)
			if resolveErr != nil {
				return nil, resolveErr
			}
			if sessionID != "" && streamInfo != "" {
				line, err = p.variantURL(sessionID, qualityName(streamInfo), resolved, referer)
			} else {
				line, err = p.proxyURL(resolved, referer)
			}
			streamInfo = ""
			if err != nil {
				return nil, err
			}
		} else if strings.Contains(line, "URI=") {
			var replacementErr error
			line = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
				if replacementErr != nil {
					return match
				}
				quoted := strings.TrimPrefix(match, "URI=")
				if len(quoted) < 2 {
					return match
				}
				resolved, resolveErr := resolveReference(base, quoted[1:len(quoted)-1])
				if resolveErr != nil {
					replacementErr = resolveErr
					return match
				}
				proxied, proxyErr := p.proxyURL(resolved, referer)
				if proxyErr != nil {
					replacementErr = proxyErr
					return match
				}
				return `URI="` + proxied + `"`
			})
			if replacementErr != nil {
				return nil, replacementErr
			}
		}
		output.WriteString(line)
		output.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return []byte(output.String()), nil
}

func copyProxyHeaders(destination, source http.Header) {
	for _, name := range []string{"Accept-Ranges", "Content-Range", "ETag", "Last-Modified"} {
		if value := source.Get(name); value != "" {
			destination.Set(name, value)
		}
	}
}

func isPlaylist(contentType, rawURL string, prefix []byte) bool {
	return strings.Contains(strings.ToLower(contentType), "mpegurl") ||
		strings.HasSuffix(strings.ToLower(strings.Split(rawURL, "?")[0]), ".m3u8") ||
		strings.HasPrefix(strings.TrimSpace(string(prefix)), "#EXTM3U")
}

func (p *HLSProxy) route(path string) (token, sessionID string, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(path, "/hls/"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] != "":
		return parts[0], parts[0], true // legacy /hls/{id}
	case len(parts) == 2 && parts[0] != "" && parts[1] == "master.m3u8":
		return parts[0], parts[0], true
	case len(parts) == 3 && parts[0] != "" && parts[1] != "" && parts[2] == "index.m3u8":
		key := parts[0] + "/" + parts[1]
		p.mu.RLock()
		token, found := p.variants[key]
		p.mu.RUnlock()
		return token, parts[0], found
	default:
		return "", "", false
	}
}

func (p *HLSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	token, sessionID, routed := p.route(r.URL.Path)
	if !routed {
		writeError(w, http.StatusNotFound, fmt.Errorf("HLS resource not found"))
		return
	}
	resource, found := p.resource(token)
	if !found {
		writeError(w, http.StatusGone, fmt.Errorf("HLS session expired or was not found"))
		return
	}
	request, err := http.NewRequestWithContext(r.Context(), r.Method, resource.URL, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	request.Header.Set("User-Agent", userAgent)
	request.Header.Set("Referer", resource.Referer)
	if value := r.Header.Get("Range"); value != "" {
		request.Header.Set("Range", value)
	}
	response, err := p.client.Do(request)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer response.Body.Close()

	copyProxyHeaders(w.Header(), response.Header)
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Type", contentType)
		if length := response.Header.Get("Content-Length"); length != "" {
			w.Header().Set("Content-Length", length)
		}
		w.WriteHeader(response.StatusCode)
		return
	}
	prefix := make([]byte, 16)
	count, readErr := io.ReadFull(response.Body, prefix)
	if readErr != nil && readErr != io.ErrUnexpectedEOF && readErr != io.EOF {
		writeError(w, http.StatusBadGateway, readErr)
		return
	}
	prefix = prefix[:count]
	if isPlaylist(contentType, resource.URL, prefix) {
		const maxPlaylistSize = 4 << 20
		content, err := io.ReadAll(io.LimitReader(io.MultiReader(strings.NewReader(string(prefix)), response.Body), maxPlaylistSize+1))
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		if len(content) > maxPlaylistSize {
			writeError(w, http.StatusBadGateway, fmt.Errorf("upstream HLS playlist is too large"))
			return
		}
		rewritten, err := p.rewritePlaylistForSession(content, resource.URL, resource.Referer, sessionID)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", fmt.Sprint(len(rewritten)))
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(rewritten)
		return
	}
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	if length := response.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(prefix)
	_, _ = io.Copy(w, response.Body)
}
