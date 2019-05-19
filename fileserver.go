package gzipped

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/golang/gddo/httputil/header"
)

// Encoding represents an Accept-Encoding. All of these fields are pre-populated
// in the supportedEncodings variable, except the clientPreference which is updated
// (by copying a value from supportedEncodings) when examining client headers.
type encoding struct {
	name             string  // the encoding name
	extension        string  // the file extension (including a leading dot)
	clientPreference float64 // the client's preference
	serverPreference int     // the server's preference
}

// Helper type to sort encodings, using clientPreference first, and then
// serverPreference as a tie breaker. This sorts in *DESCENDING* order, rather
// than the usual ascending order.
type encodingByPreference []encoding

// Implement the sort.Interface interface
func (e encodingByPreference) Len() int { return len(e) }
func (e encodingByPreference) Less(i, j int) bool {
	if e[i].clientPreference == e[j].clientPreference {
		return e[i].serverPreference > e[j].serverPreference
	}
	return e[i].clientPreference > e[j].clientPreference
}
func (e encodingByPreference) Swap(i, j int) { e[i], e[j] = e[j], e[i] }

// Supported encodings. Higher server preference means the encoding will be when
// the client doesn't have an explicit preference.
var supportedEncodings = [...]encoding{
	// Keep this one first
	{
		name:             "",
		extension:        "",
		serverPreference: 0,
	},
	{
		name:             "gzip",
		extension:        ".gz",
		serverPreference: 1,
	},
	{
		name:             "br",
		extension:        ".br",
		serverPreference: 2,
	},
}

type fileHandler struct {
	root http.FileSystem
}

type cacheEntry struct {
	info os.FileInfo
	data []byte
}

type cachingFileHandler struct {
	root  http.FileSystem
	cache map[string]map[string]*cacheEntry
	lock  *sync.RWMutex
}

// FlushCache clears all the cache entries
func (f *cachingFileHandler) FlushCache() {
	f.lock.Lock()
	for _, encoding := range supportedEncodings {
		f.cache[encoding.name] = make(map[string]*cacheEntry, 50)
	}
	f.lock.Unlock()
}

// FileServer is a drop-in replacement for Go's standard http.FileServer
// which adds support for static resources precompressed with gzip, at
// the cost of removing the support for directory browsing.
//
// If file filename.ext has a compressed version filename.ext.gz alongside
// it, if the client indicates that it accepts gzip-compressed data, and
// if the .gz file can be opened, then the compressed version of the file
// will be sent to the client. Otherwise the request is passed on to
// http.ServeContent, and the raw (uncompressed) version is used.
//
// It is up to you to ensure that the compressed and uncompressed versions
// of files match and have sensible timestamps.
//
// Compressed or not, requests are fulfilled using http.ServeContent, and
// details like accept ranges and content-type sniffing are handled by that
// method.
func FileServer(root http.FileSystem) http.Handler {
	return &fileHandler{root}
}

// CachingFileServer is a drop-in replacement for FileServer with in-memory
// caching
func CachingFileServer(root http.FileSystem) http.Handler {
	h := &cachingFileHandler{
		root:  root,
		cache: make(map[string]map[string]*cacheEntry, 3),
		lock:  new(sync.RWMutex),
	}
	h.FlushCache()
	return h
}

func openAndStat(root http.FileSystem, path string) (http.File, os.FileInfo, error) {
	file, err := root.Open(path)
	var info os.FileInfo
	// This slightly weird variable reuse is so we can get 100% test coverage
	// without having to come up with a test file that can be opened, yet
	// fails to stat.
	if err == nil {
		info, err = file.Stat()
	}
	if err != nil {
		return file, nil, err
	}
	if info.IsDir() {
		return file, nil, fmt.Errorf("%s is directory", path)
	}
	return file, info, nil
}

// Build a []encoding based on the Accept-Encoding header supplied by the
// client. The returned list will be sorted from most-preferred to
// least-preferred.
func acceptable(r *http.Request) []encoding {
	// list of acceptable encodings, as provided by the client
	acceptEncodings := make([]encoding, 0, len(supportedEncodings))
	acceptEncodings = append(acceptEncodings, supportedEncodings[0])

	// the quality of the * encoding; this will be -1 if not sent by client
	starQuality := -1.

	// encodings we've already seen (used to handle duplicates and *)
	seenEncodings := make(map[string]interface{})

	// match the client accept encodings against the ones we support
	for _, aspec := range header.ParseAccept(r.Header, "Accept-Encoding") {
		if _, alreadySeen := seenEncodings[aspec.Value]; alreadySeen {
			continue
		}
		seenEncodings[aspec.Value] = nil
		if aspec.Value == "*" {
			starQuality = aspec.Q
			continue
		}
		for _, known := range supportedEncodings {
			if aspec.Value == known.name && aspec.Q != 0 {
				enc := known
				enc.clientPreference = aspec.Q
				acceptEncodings = append(acceptEncodings, enc)
				break
			}
		}
	}

	// If the client sent Accept: *, add all our extra known encodings. Use
	// the quality of * as the client quality for the encoding.
	if starQuality != -1. {
		for _, known := range supportedEncodings {
			if _, seen := seenEncodings[known.name]; !seen {
				enc := known
				enc.clientPreference = starQuality
				acceptEncodings = append(acceptEncodings, enc)
			}
		}
	}

	// sort the encoding based on client/server preference
	sort.Sort(encodingByPreference(acceptEncodings))
	return append(acceptEncodings)
}

// Find the best file to serve based on the client's Accept-Encoding, and which
// files actually exist on the filesystem. If no file was found that can satisfy
// the request, the error field will be non-nil.
func (f *fileHandler) findBestFile(w http.ResponseWriter, r *http.Request, fpath string) (http.File, os.FileInfo, error) {
	// find the best matching file
	for _, enc := range acceptable(r) {
		if file, info, err := openAndStat(f.root, fpath+enc.extension); err == nil {
			w.Header().Set("Content-Encoding", enc.name)
			return file, info, nil
		}
	}

	// if nothing found, try the base file with no content-encoding
	return openAndStat(f.root, fpath)
}

func (f *fileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	fpath := path.Clean(upath)
	if strings.HasSuffix(fpath, "/") {
		// If you wanted to put back directory browsing support, this is
		// where you'd do it.
		http.NotFound(w, r)
		return
	}

	// Find the best acceptable file, including trying uncompressed
	if file, info, err := f.findBestFile(w, r, fpath); err == nil {
		http.ServeContent(w, r, fpath, info.ModTime(), file)
		_ = file.Close()
		return
	}

	// Doesn't exist, compressed or uncompressed
	http.NotFound(w, r)
}

// Find the best file to serve based on the client's Accept-Encoding, and which
// files actually exist on the filesystem. If no file was found that can satisfy
// the request, the error field will be non-nil.
func (f *cachingFileHandler) findBestFile(w http.ResponseWriter, r *http.Request, fpath string) (io.ReadSeeker, os.FileInfo, error) {
	// Check if an acceptable encoding is located in the cache
	f.lock.RLock()
	for _, enc := range acceptable(r) {
		encodingCache := f.cache[enc.name]
		if entry, exists := encodingCache[fpath]; exists {
			w.Header().Set("Content-Encoding", enc.name)
			reader := bytes.NewReader(entry.data)
			f.lock.RUnlock()
			return reader, entry.info, nil
		}
	}

	// Check the file system and store in the cache
	for _, enc := range acceptable(r) {
		if file, info, err := openAndStat(f.root, fpath+enc.extension); err == nil {
			f.lock.Lock()
			encodingCache := f.cache[enc.name]
			data, err := ioutil.ReadAll(file)
			_ = file.Close()
			if err != nil {
				f.lock.Unlock()
				return nil, nil, err
			}
			entry := &cacheEntry{
				info: info,
				data: data,
			}
			encodingCache[fpath] = entry
			f.lock.Unlock()
			if enc.name != "" {
				w.Header().Set("Content-Encoding", enc.name)
			}
			return bytes.NewReader(entry.data), info, nil
		}
	}

	return nil, nil, os.ErrNotExist

}

func (f *cachingFileHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	upath := r.URL.Path
	if !strings.HasPrefix(upath, "/") {
		upath = "/" + upath
		r.URL.Path = upath
	}
	fpath := path.Clean(upath)
	if strings.HasSuffix(fpath, "/") {
		// If you wanted to put back directory browsing support, this is
		// where you'd do it.
		http.NotFound(w, r)
		return
	}

	// Find the best acceptable file, including trying uncompressed
	if file, info, err := f.findBestFile(w, r, fpath); err == nil {
		http.ServeContent(w, r, fpath, info.ModTime(), file)
		return
	}

	// Doesn't exist, compressed or uncompressed
	http.NotFound(w, r)
}
