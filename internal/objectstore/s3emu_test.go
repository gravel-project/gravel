package objectstore_test

import (
	"bufio"
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// s3emu speaks just enough of S3's path-style API for the S3 store: PUT, HEAD, GET and DELETE of
// an object, and the multipart upload minio-go uses for an unknown or large size. It checks that
// a request is signed with the expected access key but not the signature itself, and it serves
// a presigned GET's response-content-disposition the way S3 does.
type s3emu struct {
	bucket, accessKey string

	mu      sync.Mutex
	objects map[string]emuObject
	uploads map[string]*emuUpload
	nextID  int
	// requests counts requests by method, so a test can tell the network was not used.
	requests map[string]int
	// completes counts completed multipart uploads.
	completes int
}

func (e *s3emu) completed() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.completes
}

// pending is the number of multipart uploads neither completed nor aborted.
func (e *s3emu) pending() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.uploads)
}

type emuObject struct {
	data         []byte
	contentType  string
	lastModified time.Time
}

type emuUpload struct {
	key, contentType string
	parts            map[int][]byte
}

func newS3Emu(bucket, accessKey string) *s3emu {
	return &s3emu{
		bucket:    bucket,
		accessKey: accessKey,
		objects:   map[string]emuObject{},
		uploads:   map[string]*emuUpload{},
		requests:  map[string]int{},
	}
}

func (e *s3emu) count() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, c := range e.requests {
		n += c
	}
	return n
}

func (e *s3emu) keys() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	var keys []string
	for k := range e.objects {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func (e *s3emu) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	e.mu.Lock()
	e.requests[r.Method]++
	e.mu.Unlock()

	credential := r.URL.Query().Get("X-Amz-Credential")
	if credential == "" {
		credential = r.Header.Get("Authorization")
	}
	if !strings.Contains(credential, e.accessKey+"/") {
		emuError(w, http.StatusForbidden, "AccessDenied", "not signed with the expected key")
		return
	}
	key, ok := strings.CutPrefix(r.URL.Path, "/"+e.bucket+"/")
	if !ok || key == "" {
		emuError(w, http.StatusNotImplemented, "NotImplemented", "only object requests in "+e.bucket)
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		e.initiate(w, r, key)
	case r.Method == http.MethodPut && q.Has("uploadId"):
		e.uploadPart(w, r, q.Get("uploadId"), q.Get("partNumber"))
	case r.Method == http.MethodPost && q.Has("uploadId"):
		e.complete(w, r, key, q.Get("uploadId"))
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		e.mu.Lock()
		delete(e.uploads, q.Get("uploadId"))
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		e.put(w, r, key)
	case r.Method == http.MethodHead, r.Method == http.MethodGet:
		e.get(w, r, key)
	case r.Method == http.MethodDelete:
		e.mu.Lock()
		delete(e.objects, key)
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	default:
		emuError(w, http.StatusNotImplemented, "NotImplemented", r.Method)
	}
}

func (e *s3emu) put(w http.ResponseWriter, r *http.Request, key string) {
	data, err := emuBody(r)
	if err != nil {
		emuError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	e.mu.Lock()
	e.objects[key] = emuObject{data: data, contentType: emuContentType(r), lastModified: time.Now()}
	e.mu.Unlock()
	w.Header().Set("ETag", etag(data))
}

func (e *s3emu) get(w http.ResponseWriter, r *http.Request, key string) {
	e.mu.Lock()
	o, ok := e.objects[key]
	e.mu.Unlock()
	if !ok {
		if r.Method == http.MethodHead {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		emuError(w, http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		return
	}
	h := w.Header()
	h.Set("Content-Type", o.contentType)
	h.Set("Content-Length", strconv.Itoa(len(o.data)))
	h.Set("Last-Modified", o.lastModified.UTC().Format(http.TimeFormat))
	h.Set("ETag", etag(o.data))
	if cd := r.URL.Query().Get("response-content-disposition"); cd != "" {
		h.Set("Content-Disposition", cd)
	}
	if r.Method == http.MethodGet {
		_, _ = w.Write(o.data)
	}
}

func (e *s3emu) initiate(w http.ResponseWriter, r *http.Request, key string) {
	e.mu.Lock()
	e.nextID++
	id := fmt.Sprintf("upload-%d", e.nextID)
	e.uploads[id] = &emuUpload{key: key, contentType: emuContentType(r), parts: map[int][]byte{}}
	e.mu.Unlock()
	emuXML(w, struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string
		Key      string
		UploadID string `xml:"UploadId"`
	}{Bucket: e.bucket, Key: key, UploadID: id})
}

func (e *s3emu) uploadPart(w http.ResponseWriter, r *http.Request, id, number string) {
	n, err := strconv.Atoi(number)
	if err != nil || n < 1 {
		emuError(w, http.StatusBadRequest, "InvalidArgument", "partNumber")
		return
	}
	data, err := emuBody(r)
	if err != nil {
		emuError(w, http.StatusBadRequest, "IncompleteBody", err.Error())
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.uploads[id]
	if !ok {
		emuError(w, http.StatusNotFound, "NoSuchUpload", id)
		return
	}
	u.parts[n] = data
	w.Header().Set("ETag", etag(data))
}

func (e *s3emu) complete(w http.ResponseWriter, r *http.Request, key, id string) {
	var req struct {
		Parts []struct {
			PartNumber int
		} `xml:"Part"`
	}
	if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
		emuError(w, http.StatusBadRequest, "MalformedXML", err.Error())
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	u, ok := e.uploads[id]
	if !ok || u.key != key {
		emuError(w, http.StatusNotFound, "NoSuchUpload", id)
		return
	}
	var data []byte
	for i, p := range req.Parts {
		part, ok := u.parts[p.PartNumber]
		if !ok || (i > 0 && p.PartNumber <= req.Parts[i-1].PartNumber) {
			emuError(w, http.StatusBadRequest, "InvalidPart", strconv.Itoa(p.PartNumber))
			return
		}
		data = append(data, part...)
	}
	delete(e.uploads, id)
	e.completes++
	e.objects[key] = emuObject{data: data, contentType: u.contentType, lastModified: time.Now()}
	emuXML(w, struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string
		Key     string
		ETag    string
	}{Bucket: e.bucket, Key: key, ETag: etag(data)})
}

// emuBody is a request's payload, decoding the aws-chunked framing minio-go uses for a signed
// stream over plain HTTP: "<hex size>;chunk-signature=...\r\n<data>\r\n", ending with size 0.
func emuBody(r *http.Request) ([]byte, error) {
	if !strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("chunk header: %w", err)
		}
		hexSize, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(hexSize, 16, 64)
		if err != nil {
			return nil, fmt.Errorf("chunk size %q: %w", hexSize, err)
		}
		if size == 0 {
			break
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, fmt.Errorf("chunk data: %w", err)
		}
		if _, err := br.Discard(2); err != nil {
			return nil, fmt.Errorf("chunk end: %w", err)
		}
	}
	if want := r.Header.Get("X-Amz-Decoded-Content-Length"); want != "" && want != strconv.Itoa(out.Len()) {
		return nil, fmt.Errorf("decoded %d bytes, want %s", out.Len(), want)
	}
	return out.Bytes(), nil
}

func emuContentType(r *http.Request) string {
	if ct := r.Header.Get("Content-Type"); ct != "" {
		return ct
	}
	return "binary/octet-stream"
}

func etag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func emuXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_ = xml.NewEncoder(w).Encode(v)
}

func emuError(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(struct {
		XMLName xml.Name `xml:"Error"`
		Code    string
		Message string
	}{Code: code, Message: msg})
}
