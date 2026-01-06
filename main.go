package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/akamensky/argparse"
	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/valyala/gozstd"
)

var (
	fileCompressionDuration = promauto.NewSummary(prometheus.SummaryOpts{
		Name:       "html_storage_file_compression_duration_seconds",
		Help:       "Duration of file compression operations",
		Objectives: map[float64]float64{0.5: 0.05, 0.9: 0.01, 0.99: 0.001},
	})
)

var cdict *gozstd.CDict

func compressHTML(html string) []byte {
	//var buf bytes.Buffer
	//gz := gzip.NewWriter(&buf)
	//_, _ = gz.Write([]byte(html))
	//_ = gz.Close()

	start := time.Now()
	defer func() {
		fileCompressionDuration.Observe(time.Since(start).Seconds())
	}()

	compressedData := gozstd.CompressDict(nil, []byte(html), cdict)

	return compressedData
}

func main() {
	gin.SetMode(gin.ReleaseMode)

	parser := argparse.NewParser("geulgyeol-html-precompressor", "A HTML pre-compressing server for Geulgyeol.")

	port := parser.Int("p", "port", &argparse.Options{Default: 8080, Help: "Port to run the server on"})
	originalEndpoint := parser.String("o", "original-endpoint", &argparse.Options{Default: "http://html-storage.default.svc.cluster.local", Help: "Original HTML storage server endpoint"})
	zstdDictionaryPath := parser.String("z", "zstd-dictionary", &argparse.Options{Default: "./zstd_dict", Help: "Path to Zstd dictionary file"})

	err := parser.Parse(os.Args)
	if err != nil {
		panic(err)
	}

	// Load Zstd dictionary
	dictData, err := os.ReadFile(*zstdDictionaryPath)
	if err != nil {
		panic(fmt.Sprintf("Failed to read Zstd dictionary: %v", err))
	}

	cdict, err = gozstd.NewCDictLevel(dictData, 9)
	if err != nil {
		panic(fmt.Sprintf("Failed to create Zstd dictionary: %v", err))
	}

	client := &http.Client{
		Timeout: 120 * time.Second,
	}

	r := gin.Default()

	// Prometheus metrics endpoint
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	r.GET("/", func(c *gin.Context) {
		c.JSON(200, gin.H{"status": "ok"})
	})

	r.POST("/:id", func(c *gin.Context) {
		var body struct {
			Body      string `json:"body"`
			Blog      string `json:"blog"`
			Timestamp int64  `json:"timestamp"`
		}

		if err := c.BindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "Invalid JSON"})
			return
		}

		go func() {
			compressedHTML := compressHTML(body.Body)

			// send to original endpoint
			reqBody := map[string]interface{}{
				"body":      base64.StdEncoding.EncodeToString(compressedHTML),
				"blog":      body.Blog,
				"timestamp": body.Timestamp,
			}
			jsonData, _ := json.Marshal(reqBody)

			resp, err := client.Post(fmt.Sprintf("%s/%s?is_precompressed=true", *originalEndpoint, c.Param("id")), "application/json", bytes.NewBuffer(jsonData))
			if err != nil {
				fmt.Printf("Error sending to original endpoint: %v\n", err)
				return
			}
			defer func(Body io.ReadCloser) {
				_ = Body.Close()
			}(resp.Body)

			if resp.StatusCode != http.StatusOK {
				fmt.Printf("Original endpoint returned non-OK status: %d\n", resp.StatusCode)
			}
		}()

		c.JSON(200, gin.H{"status": "success"})
	})

	r.POST("/batch", func(c *gin.Context) {
		var body map[string]struct {
			Body      string `json:"body"`
			Blog      string `json:"blog"`
			Timestamp int64  `json:"timestamp"`
		}

		if err := c.BindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "Invalid JSON"})
			return
		}

		// compress all

		var compressedBodies = make(map[string]map[string]interface{})

		for id, item := range body {
			compressedHTML := compressHTML(item.Body)

			compressedBodies[id] = map[string]interface{}{
				"body":      base64.StdEncoding.EncodeToString(compressedHTML),
				"blog":      item.Blog,
				"timestamp": item.Timestamp,
			}
		}

		// send to original endpoint

		jsonData, _ := json.Marshal(compressedBodies)

		resp, err := client.Post(fmt.Sprintf("%s/batch?is_precompressed=true", *originalEndpoint), "application/json", bytes.NewBuffer(jsonData))
		if err != nil {
			fmt.Printf("Error sending to original endpoint: %v\n", err)
			c.JSON(500, gin.H{"error": "Failed to send to original endpoint"})
			return
		}
		defer func(Body io.ReadCloser) {
			_ = Body.Close()
		}(resp.Body)

		if resp.StatusCode != http.StatusOK {
			fmt.Printf("Original endpoint returned non-OK status: %d\n", resp.StatusCode)
			c.JSON(500, gin.H{"error": "Original endpoint returned non-OK status"})
			return
		}

		c.JSON(200, gin.H{"status": "success"})
	})

	fmt.Printf("Starting server on port %d\n", *port)

	// run the server
	_ = r.Run(fmt.Sprintf(":%d", *port))
}
