package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	passphrase = "nanokvm-sipeed-2024" // per NanoKVM UI/client
)

type Config struct {
	BaseURL  string // e.g. http://127.0.0.1
	Username string
	Password string
	Listen   string // e.g. 127.0.0.1:18080
}

type loginResp struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data struct {
		Token string `json:"token"`
	} `json:"data"`
}

type jwtPayload struct {
	Exp int64 `json:"exp"`
}

type TokenManager struct {
	cfg Config

	mu       sync.Mutex
	token    string
	exp      int64
	lastErr  error
	httpc    *http.Client
	skewSecs int64
}

func NewTokenManager(cfg Config) *TokenManager {
	return &TokenManager{
		cfg: cfg,
		httpc: &http.Client{
			Timeout: 5 * time.Second,
			Transport: &http.Transport{
				Proxy: nil,
				DialContext: (&net.Dialer{
					Timeout:   3 * time.Second,
					KeepAlive: 30 * time.Second,
				}).DialContext,
				MaxIdleConns:          10,
				MaxIdleConnsPerHost:   10,
				IdleConnTimeout:       60 * time.Second,
				ResponseHeaderTimeout: 5 * time.Second,
			},
		},
		skewSecs: 60,
	}
}

func (tm *TokenManager) Get(ctx context.Context) (string, error) {
	tm.mu.Lock()
	defer tm.mu.Unlock()

	now := time.Now().Unix()
	if tm.token != "" && tm.exp != 0 && (tm.exp-now) > tm.skewSecs {
		return tm.token, nil
	}

	tok, exp, err := tm.login(ctx)
	if err != nil {
		tm.lastErr = err
		return "", err
	}
	tm.token = tok
	tm.exp = exp
	tm.lastErr = nil
	return tok, nil
}

func (tm *TokenManager) Invalidate() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.token = ""
	tm.exp = 0
}

func (tm *TokenManager) login(ctx context.Context) (token string, exp int64, err error) {
	encPass, err := encryptPasswordOpenSSLCompat(tm.cfg.Password, passphrase)
	if err != nil {
		return "", 0, err
	}

	body := fmt.Sprintf(`{"username":"%s","password":"%s"}`, jsonEscape(tm.cfg.Username), encPass)

	req, err := http.NewRequestWithContext(ctx, "POST", tm.cfg.BaseURL+"/api/auth/login", strings.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := tm.httpc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return "", 0, fmt.Errorf("login http %d: %s", resp.StatusCode, string(b))
	}

	var lr loginResp
	if err := json.Unmarshal(b, &lr); err != nil {
		return "", 0, fmt.Errorf("login decode: %w; body=%s", err, string(b))
	}
	if lr.Code != 0 || lr.Data.Token == "" {
		return "", 0, fmt.Errorf("login failed code=%d msg=%s", lr.Code, lr.Msg)
	}

	exp, err = parseJWTExp(lr.Data.Token)
	if err != nil {
		// still usable; just force refresh sooner
		exp = time.Now().Add(5 * time.Minute).Unix()
	}
	return lr.Data.Token, exp, nil
}

// --- MJPEG reader ---

type FrameStore struct {
	mu    sync.RWMutex
	frame []byte
	at    time.Time
}

func (fs *FrameStore) Set(jpeg []byte) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	// copy to avoid holding huge backing buffers
	fs.frame = append(fs.frame[:0], jpeg...)
	fs.at = time.Now()
}
func (fs *FrameStore) Get() (jpeg []byte, at time.Time) {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.frame == nil {
		return nil, time.Time{}
	}
	out := make([]byte, len(fs.frame))
	copy(out, fs.frame)
	return out, fs.at
}

func streamMJPEG(ctx context.Context, cfg Config, tm *TokenManager, fs *FrameStore) {
	httpc := &http.Client{
		Timeout: 0, // streaming
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: (&net.Dialer{
				Timeout:   5 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			MaxIdleConns:        2,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     60 * time.Second,
		},
	}

	backoff := 200 * time.Millisecond
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		tok, err := tm.Get(ctx)
		if err != nil {
			log.Printf("token err: %v", err)
			time.Sleep(backoff)
			continue
		}

		req, _ := http.NewRequestWithContext(ctx, "GET", cfg.BaseURL+"/api/stream/mjpeg", nil)
		req.Header.Set("Cookie", "nano-kvm-token="+tok)

		resp, err := httpc.Do(req)
		if err != nil {
			log.Printf("mjpeg connect err: %v", err)
			time.Sleep(backoff)
			continue
		}

		if resp.StatusCode == 401 {
			resp.Body.Close()
			tm.Invalidate()
			time.Sleep(100 * time.Millisecond)
			continue
		}
		if resp.StatusCode != 200 {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			log.Printf("mjpeg http %d: %s", resp.StatusCode, string(b))
			time.Sleep(backoff)
			continue
		}

		ct := resp.Header.Get("Content-Type")
		if !strings.Contains(strings.ToLower(ct), "multipart/x-mixed-replace") {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			log.Printf("not mjpeg content-type=%q body=%q", ct, string(b))
			time.Sleep(backoff)
			continue
		}

		log.Printf("MJPEG connected (%s)", ct)
		if err := readMJPEGFrames(ctx, resp.Body, fs); err != nil {
			log.Printf("mjpeg read err: %v", err)
		}
		resp.Body.Close()

		time.Sleep(100 * time.Millisecond)
	}
}

// Parses frames like:
// --frame\r\nContent-Type: image/jpeg\r\nContent-Length: N\r\n\r\n<jpeg bytes>\r\n
func readMJPEGFrames(ctx context.Context, r io.Reader, fs *FrameStore) error {
	br := bufio.NewReaderSize(r, 256*1024)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// Read boundary line
		line, err := br.ReadString('\n')
		if err != nil {
			return err
		}
		if !strings.HasPrefix(line, "--") {
			// keep scanning
			continue
		}

		// Read headers until blank line
		var contentLen int
		for {
			h, err := br.ReadString('\n')
			if err != nil {
				return err
			}
			h = strings.TrimRight(h, "\r\n")
			if h == "" { // end headers
				break
			}
			parts := strings.SplitN(h, ":", 2)
			if len(parts) != 2 {
				continue
			}
			k := strings.ToLower(strings.TrimSpace(parts[0]))
			v := strings.TrimSpace(parts[1])
			if k == "content-length" {
				n, _ := strconv.Atoi(v)
				contentLen = n
			}
		}

		if contentLen <= 0 || contentLen > 10*1024*1024 {
			// If missing content-length, fallback: scan JPEG markers (slower).
			return errors.New("missing/invalid content-length in MJPEG part")
		}

		jpeg := make([]byte, contentLen)
		if _, err := io.ReadFull(br, jpeg); err != nil {
			return err
		}

		// Consume trailing newline(s) after jpeg if present
		_, _ = br.ReadByte()
		// best-effort put it back if not newline
		// (we ignore; stream keeps going)

		// Validate JPEG SOI/EOI quickly
		if len(jpeg) >= 4 && jpeg[0] == 0xFF && jpeg[1] == 0xD8 {
			fs.Set(jpeg)
		}
	}
}

// --- Crypto (OpenSSL compatible AES-256-CBC with Salted__, EVP_BytesToKey MD5) ---

func encryptPasswordOpenSSLCompat(plain, pass string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	key, iv := evpBytesToKeyMD5([]byte(pass), salt, 32, 16)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	padded := pkcs7Pad([]byte(plain), aes.BlockSize)

	ct := make([]byte, len(padded))
	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(ct, padded)

	blob := append([]byte("Salted__"), salt...)
	blob = append(blob, ct...)

	b64 := base64.StdEncoding.EncodeToString(blob)
	// NanoKVM client URL-escapes this
	return url.QueryEscape(b64), nil
}

func evpBytesToKeyMD5(pass, salt []byte, keyLen, ivLen int) ([]byte, []byte) {
	var d, prev []byte
	for len(d) < keyLen+ivLen {
		h := md5.New()
		h.Write(prev)
		h.Write(pass)
		h.Write(salt)
		prev = h.Sum(nil)
		d = append(d, prev...)
	}
	return d[:keyLen], d[keyLen : keyLen+ivLen]
}

func pkcs7Pad(b []byte, blockSize int) []byte {
	pad := blockSize - (len(b) % blockSize)
	return append(b, bytes.Repeat([]byte{byte(pad)}, pad)...)
}

func parseJWTExp(token string) (int64, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return 0, errors.New("invalid jwt")
	}
	payload := parts[1]
	payload = strings.ReplaceAll(payload, "-", "+")
	payload = strings.ReplaceAll(payload, "_", "/")
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	raw, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return 0, err
	}
	var p jwtPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return 0, err
	}
	if p.Exp == 0 {
		return 0, errors.New("no exp in jwt")
	}
	return p.Exp, nil
}

func jsonEscape(s string) string {
	// minimal safe escape for embedding in JSON string with fmt.Sprintf
	b, _ := json.Marshal(s)
	// b is like "text"
	return strings.Trim(string(b), `"`)
}

func loadConf(path string) (user, pass string, err error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	lines := strings.Split(string(data), "\n")
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "KVM_USER=") {
			user = strings.TrimPrefix(l, "KVM_USER=")
		}
		if strings.HasPrefix(l, "KVM_PASS=") {
			pass = strings.TrimPrefix(l, "KVM_PASS=")
		}
	}
	if user == "" || pass == "" {
		return "", "", fmt.Errorf("missing KVM_USER or KVM_PASS in %s", path)
	}
	return user, pass, nil
}

// --- HTTP server ---

func main() {
	confPath := getenv("SNAP_CONF", "/etc/nanokvm.snapshot.conf")

	user, pass, err := loadConf(confPath)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	cfg := Config{
		BaseURL:  getenv("NANOKVM_BASE_URL", "http://127.0.0.1"),
		Username: user,
		Password: pass,
		Listen:   getenv("SNAP_LISTEN", "127.0.0.1:18080"),
	}

	tm := NewTokenManager(cfg)
	fs := &FrameStore{}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go streamMJPEG(ctx, cfg, tm, fs)

	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot.jpg", func(w http.ResponseWriter, r *http.Request) {
		jpeg, _ := fs.Get()
		if jpeg == nil {
			http.Error(w, "no frame yet", 503)
			return
		}
		w.Header().Set("Content-Type", "image/jpeg")
		w.Write(jpeg)
	})

	log.Printf("listening on http://%s/snapshot.jpg", cfg.Listen)
	log.Fatal(http.ListenAndServe(cfg.Listen, mux))
}

func getenv(k, def string) string {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	return v
}

func mustGetenv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env %s", k)
	}
	return v
}
