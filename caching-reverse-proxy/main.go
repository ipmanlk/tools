package main

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// Configuration constants.
const (
	// Custom header for authentication.
	authHeaderName = "X-TWC-Cache-Auth"
	// Custom header to indicate cache hits
	cacheHitHeaderName = "X-TWC-From-Cache"

	// Query parameters to control caching.
	targetURLParam   = "twc_url"
	skipCacheParam   = "twc_skip_cache"   // optional
	cacheExpiryParam = "twc_cache_expiry" // in seconds; optional
	timeoutParam     = "twc_timeout"      // in seconds; optional

	// Data configuration.
	dbDirName  = "data"
	dbFileName = "cache.db"
)

var (
	db              *sql.DB
	authHeaderValue string
)

func init() {
	// Load the API key from the environment variable or use a default value.
	authHeaderValue = os.Getenv("CACHE_API_KEY")
	if authHeaderValue == "" {
		authHeaderValue = "testing"
	}
}

func main() {
	var err error
	// Create the data directory if it does not exist.
	if err = os.MkdirAll(dbDirName, 0755); err != nil {
		log.Fatal("Error creating data directory:", err)
	}

	// Open existing or create a new SQLite database.
	db, err = sql.Open("sqlite3", dbDirName+"/"+dbFileName)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	// Configure SQLite for high concurrency.
	if _, err = db.Exec("PRAGMA journal_mode=WAL;"); err != nil {
		log.Fatal("Error setting WAL mode:", err)
	}
	if _, err = db.Exec("PRAGMA synchronous=NORMAL;"); err != nil {
		log.Fatal("Error setting synchronous mode:", err)
	}
	if _, err = db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		log.Fatal("Error setting busy timeout:", err)
	}

	// Create cache table if it does not exist.
	createTableSQL := `
	CREATE TABLE IF NOT EXISTS cache (
		key TEXT PRIMARY KEY,
		response TEXT,
		headers TEXT,
		status_code INTEGER,
		created_at TIMESTAMP,
		expires_at TIMESTAMP
	);`
	if _, err = db.Exec(createTableSQL); err != nil {
		log.Fatal(err)
	}

	http.HandleFunc("/", handler)
	log.Println("Server started on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handler(w http.ResponseWriter, r *http.Request) {
	// Enforce the unique auth header.
	if r.Header.Get(authHeaderName) != authHeaderValue {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Extract the target URL from the query parameter.
	targetURL := r.URL.Query().Get(targetURLParam)
	if targetURL == "" {
		http.Error(w, "Missing "+targetURLParam+" parameter", http.StatusBadRequest)
		return
	}
	if _, err := url.ParseRequestURI(targetURL); err != nil {
		http.Error(w, "Invalid "+targetURLParam+" parameter", http.StatusBadRequest)
		return
	}

	// Determine whether to skip the cache.
	skipCache := r.URL.Query().Get(skipCacheParam) == "true"

	// Optional cache expiry in seconds.
	var expiryTime *time.Time
	if expiryStr := r.URL.Query().Get(cacheExpiryParam); expiryStr != "" {
		secs, err := strconv.Atoi(expiryStr)
		if err != nil || secs < 0 {
			http.Error(w, "Invalid "+cacheExpiryParam+" parameter", http.StatusBadRequest)
			return
		}
		t := time.Now().Add(time.Duration(secs) * time.Second)
		expiryTime = &t
	}

	// Read the request body (if any) for forwarding and as part of the cache key.
	var reqBody []byte
	if r.Body != nil {
		var err error
		reqBody, err = io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "Error reading request body: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	// Compute a cache key based on the method, target URL, and request body.
	cacheKey := computeCacheKey(r.Method, targetURL, reqBody)

	// Check cache unless skipCache is requested.
	if !skipCache {
		var cachedResp string
		var cachedHeaders string
		var cachedStatusCode int
		err := db.QueryRow("SELECT response, headers, status_code FROM cache WHERE key = ? AND (expires_at IS NULL OR expires_at > ?)", cacheKey, time.Now()).Scan(&cachedResp, &cachedHeaders, &cachedStatusCode)
		if err == nil {
			// Found valid cached response.
			// Set our custom header to indicate the response comes from cache.
			w.Header().Set(cacheHitHeaderName, "true")
			// Restore allowed headers.
			var headerMap map[string][]string
			if err := json.Unmarshal([]byte(cachedHeaders), &headerMap); err != nil {
				log.Printf("Error unmarshaling cached headers: %v", err)
			} else {
				for name, values := range headerMap {
					for _, v := range values {
						w.Header().Add(name, v)
					}
				}
			}
			w.WriteHeader(cachedStatusCode)
			w.Write([]byte(cachedResp))
			return
		}
	}

	// Create the forwarded request.
	forwardReq, err := http.NewRequest(r.Method, targetURL, bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "Error creating request: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Set request timeout
	timeout := 15 * time.Second // default timeout
	if timeoutStr := r.URL.Query().Get(timeoutParam); timeoutStr != "" {
		secs, err := strconv.Atoi(timeoutStr)
		if err != nil || secs < 5 || secs > 120 {
			http.Error(w, "Invalid "+timeoutParam+" parameter (must be between 5 and 120 seconds)", http.StatusBadRequest)
			return
		}
		timeout = time.Duration(secs) * time.Second
	}

	// Forward all headers except the auth header.
	for name, values := range r.Header {
		if name == authHeaderName {
			continue
		}
		for _, v := range values {
			forwardReq.Header.Add(name, v)
		}
	}

	// Send the forwarded request.
	client := &http.Client{Timeout: timeout}
	resp, err := client.Do(forwardReq)
	if err != nil {
		http.Error(w, "Error forwarding request: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Read the response body.
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, "Error reading response: "+err.Error(), http.StatusInternalServerError)
		return
	}

	// Copy response headers to the client.
	for name, values := range resp.Header {
		for _, v := range values {
			w.Header().Add(name, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	w.Write(respBody)

	// Cache the response if not skipping cache.
	if !skipCache {
		// Filter allowed headers.
		allowedKeys := []string{
			"Content-Type", "Content-Encoding", "Cache-Control",
			"Expires", "ETag", "Last-Modified", "Vary",
		}
		filteredHeaders := make(map[string][]string)
		for _, key := range allowedKeys {
			if vals, ok := resp.Header[key]; ok {
				filteredHeaders[key] = vals
			}
		}

		// Serialize filtered headers to JSON.
		headersJSON, err := json.Marshal(filteredHeaders)
		if err != nil {
			log.Printf("Error marshaling response headers: %v", err)
		}

		var execErr error
		if expiryTime != nil {
			_, execErr = db.Exec("INSERT OR REPLACE INTO cache (key, response, headers, status_code, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)",
				cacheKey, string(respBody), string(headersJSON), resp.StatusCode, time.Now(), *expiryTime)
		} else {
			_, execErr = db.Exec("INSERT OR REPLACE INTO cache (key, response, headers, status_code, created_at, expires_at) VALUES (?, ?, ?, ?, ?, NULL)",
				cacheKey, string(respBody), string(headersJSON), resp.StatusCode, time.Now())
		}
		if execErr != nil {
			log.Printf("Error caching response: %v", execErr)
		}
	}
}

// computeCacheKey returns a SHA256 hash string based on method, URL, and request body.
func computeCacheKey(method, urlStr string, body []byte) string {
	h := sha256.New()
	h.Write([]byte(method))
	h.Write([]byte(urlStr))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
