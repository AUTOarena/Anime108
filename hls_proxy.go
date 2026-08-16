package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	entryPlaylistName      = "playlist.m3u8"
	maxResourcesPerSession = 20000
	maxSessions            = 512

	kindIndex   = "index"
	kindSegment = "segment"
	kindKey     = "key"
	kindInit    = "init"
)

// hlsSession groups every upstream resource that belongs to one playback
// session. Resources are addressed by a generated file name so that the proxy
// exposes player friendly paths such as /hls/{id}/segment-12.ts while the
// upstream URL itself is never revealed to the client.
type hlsSession struct {
	id       string
	entryURL string
	referer  string

	mu        sync.Mutex
	expiresAt time.Time
	byName    map[string]string
	byURL     map[string]string
	counter   int
}

func newHLSSession(id, entryURL, referer string, expiresAt time.Time) *hlsSession {
	session := &hlsSession{
		id:        id,
		entryURL:  entryURL,
		referer:   referer,
		expiresAt: expiresAt,
		byName:    map[string]string{entryPlaylistName: entryURL},
		byURL:     map[string]string{entryURL: entryPlaylistName},
	}
	return session
}

func (s *hlsSession) alive(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return now.Before(s.expiresAt)
}

func (s *hlsSession) touch(expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if expiresAt.After(s.expiresAt) {
		s.expiresAt = expiresAt
	}
}

func (s *hlsSession) deadline() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.expiresAt
}

func (s *hlsSession) lookup(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	target, found := s.byName[name]
	return target, found
}

func (s *hlsSession) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byName)
}

// nameFor returns the stable proxy file name for an upstream URL, allocating a
// new one the first time the URL is seen inside this session.
func (s *hlsSession) nameFor(rawURL, kind string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if name, found := s.byURL[rawURL]; found {
		return name, nil
	}
	if len(s.byName) >= maxResourcesPerSession {
		return "", fmt.Errorf("HLS session resource limit reached")
	}
	s.counter++
	name := fmt.Sprintf("%s-%d%s", kind, s.counter, resourceExtension(rawURL, kind))
	s.byName[name] = rawURL
	s.byURL[rawURL] = name
	return name, nil
}

type HLSProxy struct {
	client   *http.Client
	ttl      time.Duration
	mu       sync.RWMutex
	sessions map[string]*hlsSession
}

func NewHLSProxy(ttl time.Duration) *HLSProxy {
	return &HLSProxy{
		client:   &http.Client{Timeout: 90 * time.Second},
		ttl:      ttl,
		sessions: make(map[string]*hlsSession),
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

var (
	safeExtension = regexp.MustCompile(`^\.[A-Za-z0-9]{1,5}$`)
	safeSessionID = regexp.MustCompile(`^[a-f0-9]{8,64}$`)
	safeName      = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	playlistURI   = regexp.MustCompile(`URI=("[^"]+"|'[^']+')`)
)

func resourceExtension(rawURL, kind string) string {
	extension := ""
	if parsed, err := url.Parse(rawURL); err == nil {
		extension = strings.ToLower(path.Ext(parsed.Path))
	}
	if safeExtension.MatchString(extension) {
		return extension
	}
	switch kind {
	case kindIndex:
		return ".m3u8"
	case kindKey:
		return ".key"
	case kindInit:
		return ".mp4"
	default:
		return ".ts"
	}
}

func contentTypeForName(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".m3u8", ".m3u":
		return "application/vnd.apple.mpegurl"
	case ".ts":
		return "video/mp2t"
	case ".mp4", ".m4s", ".m4v":
		return "video/mp4"
	case ".m4a", ".aac":
		return "audio/aac"
	case ".vtt":
		return "text/vtt"
	}
	return "application/octet-stream"
}

// CreateSession registers the entry playlist and returns the session ID used in
// /hls/{id}/playlist.m3u8.
func (p *HLSProxy) CreateSession(entryURL, referer string) (string, error) {
	if !validUpstream(entryURL) {
		return "", fmt.Errorf("invalid HLS upstream URL")
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sweepLocked(now)
	for _, session := range p.sessions {
		if session.entryURL == entryURL && session.referer == referer {
			session.touch(now.Add(p.ttl))
			return session.id, nil
		}
	}
	p.evictLocked()
	id, err := randomToken()
	if err != nil {
		return "", err
	}
	p.sessions[id] = newHLSSession(id, entryURL, referer, now.Add(p.ttl))
	return id, nil
}

// PlaylistPath is the client facing entry point of a session.
func (p *HLSProxy) PlaylistPath(id string) string {
	return "/hls/" + id + "/" + entryPlaylistName
}

func (p *HLSProxy) sweepLocked(now time.Time) {
	for id, session := range p.sessions {
		if !session.alive(now) {
			delete(p.sessions, id)
		}
	}
}

func (p *HLSProxy) evictLocked() {
	for len(p.sessions) >= maxSessions {
		oldestID := ""
		var oldest time.Time
		for id, session := range p.sessions {
			deadline := session.deadline()
			if oldestID == "" || deadline.Before(oldest) {
				oldestID, oldest = id, deadline
			}
		}
		if oldestID == "" {
			return
		}
		delete(p.sessions, oldestID)
	}
}

func (p *HLSProxy) session(id string) (*hlsSession, bool) {
	p.mu.RLock()
	session, found := p.sessions[id]
	p.mu.RUnlock()
	if !found {
		return nil, false
	}
	if !session.alive(time.Now()) {
		p.mu.Lock()
		delete(p.sessions, id)
		p.mu.Unlock()
		return nil, false
	}
	return session, true
}

func resolveReference(base *url.URL, value string) (string, error) {
	reference, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", err
	}
	return base.ResolveReference(reference).String(), nil
}

func attributeKind(tag string) string {
	switch {
	case strings.HasPrefix(tag, "#EXT-X-KEY"), strings.HasPrefix(tag, "#EXT-X-SESSION-KEY"):
		return kindKey
	case strings.HasPrefix(tag, "#EXT-X-MAP"):
		return kindInit
	case strings.HasPrefix(tag, "#EXT-X-PART"), strings.HasPrefix(tag, "#EXT-X-PRELOAD-HINT"):
		return kindSegment
	default:
		return kindIndex
	}
}

// rewritePlaylist replaces every upstream reference with a session relative
// file name, so the playlist stays valid while never leaking upstream URLs.
func (p *HLSProxy) rewritePlaylist(session *hlsSession, content []byte, sourceURL string) ([]byte, error) {
	base, err := url.Parse(sourceURL)
	if err != nil {
		return nil, err
	}
	rewrite := func(value, kind string) (string, error) {
		resolved, err := resolveReference(base, value)
		if err != nil {
			return "", err
		}
		if strings.HasSuffix(strings.ToLower(strings.Split(resolved, "?")[0]), ".m3u8") {
			kind = kindIndex
		}
		return session.nameFor(resolved, kind)
	}

	var output strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(string(content)))
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	pendingKind := kindSegment
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
		case !strings.HasPrefix(trimmed, "#"):
			name, err := rewrite(trimmed, pendingKind)
			if err != nil {
				return nil, err
			}
			pendingKind = kindSegment
			line = name
		default:
			tag := strings.ToUpper(trimmed)
			if strings.HasPrefix(tag, "#EXT-X-STREAM-INF") {
				pendingKind = kindIndex
			}
			if strings.Contains(line, "URI=") {
				kind := attributeKind(tag)
				var replacementErr error
				line = playlistURI.ReplaceAllStringFunc(line, func(match string) string {
					if replacementErr != nil {
						return match
					}
					quoted := strings.TrimPrefix(match, "URI=")
					if len(quoted) < 2 {
						return match
					}
					name, err := rewrite(quoted[1:len(quoted)-1], kind)
					if err != nil {
						replacementErr = err
						return match
					}
					return `URI="` + name + `"`
				})
				if replacementErr != nil {
					return nil, replacementErr
				}
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

func setCORSHeaders(header http.Header) {
	header.Set("Access-Control-Allow-Origin", "*")
	header.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	header.Set("Access-Control-Allow-Headers", "Range, Origin, Accept")
	header.Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges")
	header.Set("Access-Control-Max-Age", "600")
}

func isPlaylist(contentType, rawURL string, prefix []byte) bool {
	return strings.Contains(strings.ToLower(contentType), "mpegurl") ||
		strings.HasSuffix(strings.ToLower(strings.Split(rawURL, "?")[0]), ".m3u8") ||
		strings.HasPrefix(strings.TrimSpace(string(prefix)), "#EXTM3U")
}

// splitResourcePath splits /hls/{id}/{name} into its two components.
func splitResourcePath(requestPath string) (string, string, bool) {
	rest := strings.TrimPrefix(requestPath, "/hls/")
	id, name, found := strings.Cut(rest, "/")
	if !found || !safeSessionID.MatchString(id) || !safeName.MatchString(name) {
		return "", "", false
	}
	return id, name, true
}

func (p *HLSProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w.Header())
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD, OPTIONS")
		writeError(w, http.StatusMethodNotAllowed, fmt.Errorf("method not allowed"))
		return
	}
	id, name, ok := splitResourcePath(r.URL.Path)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("HLS resource not found"))
		return
	}
	session, found := p.session(id)
	if !found {
		writeError(w, http.StatusGone, fmt.Errorf("HLS session expired or was not found"))
		return
	}
	target, found := session.lookup(name)
	if !found {
		writeError(w, http.StatusNotFound, fmt.Errorf("HLS resource not found in this session"))
		return
	}
	session.touch(time.Now().Add(p.ttl))

	request, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	request.Header.Set("User-Agent", userAgent)
	if session.referer != "" {
		request.Header.Set("Referer", session.referer)
	}
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
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		w.WriteHeader(response.StatusCode)
		_, _ = io.Copy(w, response.Body)
		return
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = contentTypeForName(name)
	}
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
	if isPlaylist(contentType, target, prefix) {
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
		rewritten, err := p.rewritePlaylist(session, content, target)
		if err != nil {
			writeError(w, http.StatusBadGateway, err)
			return
		}
		w.Header().Del("Content-Range")
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Length", fmt.Sprint(len(rewritten)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(rewritten)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if length := response.Header.Get("Content-Length"); length != "" {
		w.Header().Set("Content-Length", length)
	}
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.WriteHeader(response.StatusCode)
	_, _ = w.Write(prefix)
	_, _ = io.Copy(w, response.Body)
}
