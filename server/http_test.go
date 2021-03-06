package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/buchgr/bazel-remote/cache/disk"
	"github.com/buchgr/bazel-remote/utils"
)

func TestDownloadFile(t *testing.T) {
	cacheDir := testutils.CreateTmpCacheDirs(t)
	defer os.RemoveAll(cacheDir)

	hash, err := testutils.CreateRandomFile(filepath.Join(cacheDir, "cas"), 1024)
	if err != nil {
		t.Fatal(err)
	}

	c := disk.New(cacheDir, 1024, false)
	h := NewHTTPCache(c, testutils.NewSilentLogger(), testutils.NewSilentLogger(), false)

	req, err := http.NewRequest("GET", "/cas/"+hash, bytes.NewReader([]byte{}))
	if err != nil {
		t.Error(err)
	}
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.CacheHandler)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Error("Handler returned wrong status code",
			"expected", http.StatusOK,
			"got", status,
		)
	}

	rsp := rr.Result()
	if contentLen := rsp.ContentLength; contentLen != 1024 {
		t.Error("Handler returned file with wrong content length",
			"expected", 1024,
			"got", contentLen)
	}

	hasher := sha256.New()
	io.Copy(hasher, rsp.Body)
	if actualHash := hex.EncodeToString(hasher.Sum(nil)); actualHash != hash {
		t.Error("Received the wrong content.",
			"expected hash", hash,
			"actualHash", actualHash,
		)
	}
}

func TestUploadFilesConcurrently(t *testing.T) {
	cacheDir := testutils.CreateTmpCacheDirs(t)
	defer os.RemoveAll(cacheDir)

	const NumUploads = 1000

	var requests [NumUploads]*http.Request
	for i := 0; i < NumUploads; i++ {
		data, hash := testutils.RandomDataAndHash(1024)
		r, err := http.NewRequest("PUT", "/cas/"+hash, bytes.NewReader(data))
		if err != nil {
			t.Error(err)
		}
		requests[i] = r
	}

	c := disk.New(cacheDir, 1000*1024, false)
	h := NewHTTPCache(c, testutils.NewSilentLogger(), testutils.NewSilentLogger(), false)
	handler := http.HandlerFunc(h.CacheHandler)

	var wg sync.WaitGroup
	wg.Add(len(requests))
	for _, request := range requests {
		go func(request *http.Request) {
			defer wg.Done()
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, request)

			if status := rr.Code; status != http.StatusOK {
				t.Error("Handler returned wrong status code",
					"expected", http.StatusOK,
					"got", status,
				)
			}
		}(request)
	}

	wg.Wait()

	f, err := os.Open(cacheDir)
	defer f.Close()
	if err != nil {
		t.Error(err)
	}
	files, err := f.Readdir(-1)
	if err != nil {
		t.Error(err)
	}
	var totalSize int64
	for _, fileinfo := range files {
		if !fileinfo.IsDir() {
			size := fileinfo.Size()
			if size != 1024 {
				t.Error("Expected all files to be 1024 bytes.", "Got", size)
			}
			totalSize += fileinfo.Size()
		}
	}

	// Test that purging worked and kept cache size in check.
	if upperBound := int64(NumUploads) * 1024 * 1024; totalSize > upperBound {
		t.Error("Cache size too big. Expected at most", upperBound, "got",
			totalSize)
	}
}

func TestUploadSameFileConcurrently(t *testing.T) {
	cacheDir := testutils.CreateTmpCacheDirs(t)
	defer os.RemoveAll(cacheDir)

	data, hash := testutils.RandomDataAndHash(1024)

	c := disk.New(cacheDir, 1024, false)
	h := NewHTTPCache(c, testutils.NewSilentLogger(), testutils.NewSilentLogger(), false)
	handler := http.HandlerFunc(h.CacheHandler)

	var wg sync.WaitGroup
	wg.Add(100)
	for i := 0; i < 100; i++ {
		go func() {
			defer wg.Done()

			rr := httptest.NewRecorder()

			request, err := http.NewRequest("PUT", "/cas/"+hash, bytes.NewReader(data))
			if err != nil {
				t.Error(err)
			}

			handler.ServeHTTP(rr, request)

			if status := rr.Code; status != http.StatusOK {
				resp, _ := ioutil.ReadAll(rr.Body)
				t.Error("Handler returned wrong status code",
					"expected", http.StatusOK,
					"got", status,
					string(resp),
				)
			}
		}()
	}

	wg.Wait()
}

func TestUploadCorruptedFile(t *testing.T) {
	cacheDir := testutils.CreateTmpCacheDirs(t)
	defer os.RemoveAll(cacheDir)

	data, hash := testutils.RandomDataAndHash(1024)
	corruptedData := data[:999]

	r, err := http.NewRequest("PUT", "/cas/"+hash, bytes.NewReader(corruptedData))
	if err != nil {
		t.Error(err)
	}

	c := disk.New(cacheDir, 2048, false)
	h := NewHTTPCache(c, testutils.NewSilentLogger(), testutils.NewSilentLogger(), false)
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.CacheHandler)
	handler.ServeHTTP(rr, r)

	if status := rr.Code; status != http.StatusInternalServerError {
		t.Error("Handler returned wrong status code",
			"expected ", http.StatusInternalServerError,
			"got ", status)
	}

	// Check that no file was saved in the cache
	f, err := os.Open(cacheDir)
	defer f.Close()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := f.Readdir(-1)
	if err != nil {
		t.Fatal(err)
	}
	for _, fileEntry := range entries {
		if !fileEntry.IsDir() {
			t.Error("Unexpected file in the cache ", fileEntry.Name())
		}
	}
}

func TestStatusPage(t *testing.T) {
	cacheDir := testutils.CreateTmpCacheDirs(t)
	defer os.RemoveAll(cacheDir)

	r, err := http.NewRequest("GET", "/status", bytes.NewReader([]byte{}))
	if err != nil {
		t.Fatal(err)
	}

	c := disk.New(cacheDir, 2048, false)
	h := NewHTTPCache(c, testutils.NewSilentLogger(), testutils.NewSilentLogger(), false)
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(h.StatusPageHandler)
	handler.ServeHTTP(rr, r)

	if status := rr.Code; status != http.StatusOK {
		t.Error("StatusPageHandler returned wrong status code",
			"expected ", http.StatusOK,
			"got ", status)
	}

	var data statusPageData
	err = json.Unmarshal(rr.Body.Bytes(), &data)
	if err != nil {
		t.Fatal(err)
	}

	if numFiles := data.NumFiles; numFiles != 0 {
		t.Error("StatusPageHandler returned wrong number of files",
			"expected ", 0,
			"got ", numFiles)
	}
}

func TestCacheKeyFromRequestPath(t *testing.T) {
	{
		_, _, err := cacheKeyFromRequestPath("invalid/url")
		if err == nil {
			t.Error("Failed to reject an invalid URL")
		}
	}

	const aSha256sum = "fec3be77b8aa0d307ed840581ded3d114c86f36d4914c81e33a72877020c0603"

	{
		cacheKey, shasum, err := cacheKeyFromRequestPath("cas/" + aSha256sum)
		if err != nil {
			t.Error("Failed to parse a valid CAS URL")
		}
		if cacheKey != "cas/"+aSha256sum {
			t.Error("Cache key parsed incorrectly")
		}
		if shasum != aSha256sum {
			t.Log(shasum)
			t.Error("Hashsum parsed incorrectly")
		}
	}

	{
		cacheKey, shasum, err := cacheKeyFromRequestPath("ac/" + aSha256sum)
		if err != nil {
			t.Error("Failed to parse a valid AC URL")
		}
		if cacheKey != "ac/"+aSha256sum {
			t.Error("Cache key parsed incorrectly")
		}
		if shasum != "" {
			t.Error("Hashsum parsed incorrectly")
		}
	}

	{
		cacheKey, shasum, err := cacheKeyFromRequestPath("prefix/ac/" + aSha256sum)
		if err != nil {
			t.Error("Failed to parse a valid AC URL with prefix")
		}
		if cacheKey != "ac/"+aSha256sum {
			t.Error("Cache key parsed incorrectly")
		}
		if shasum != "" {
			t.Error("Hashsum parsed incorrectly")
		}
	}
}
