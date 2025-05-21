package main

import (
	"flag"
	"fmt"
	"io/ioutil"
	"math"
	"net/http"
	"sort"
	"sync"
	"time"
)

var (
	url         string
	requests    int
	concurrency int
	method      string
	timeout     time.Duration
	retries     int
)

func init() {
	flag.StringVar(&url, "url", "", "URL to send requests to (required)")
	flag.IntVar(&requests, "n", 100, "Number of requests to make")
	flag.IntVar(&concurrency, "c", 10, "Number of concurrent workers")
	flag.StringVar(&method, "method", "GET", "HTTP method (GET, POST, etc.)")
	flag.DurationVar(&timeout, "timeout", 10*time.Second, "Request timeout")
	flag.IntVar(&retries, "retries", 3, "Number of retries per request")
}

type Result struct {
	Latency time.Duration
	Status  int
	Error   error
}

func worker(id int, jobs <-chan int, results chan<- Result, client *http.Client, wg *sync.WaitGroup) {
	defer wg.Done()
	for range jobs {
		var res Result
		var err error
		var resp *http.Response

		reqStart := time.Now()

		for attempt := 0; attempt < retries; attempt++ {
			req, _ := http.NewRequest(method, url, nil)
			resp, err = client.Do(req)
			if err == nil {
				_, _ = ioutil.ReadAll(resp.Body)
				_ = resp.Body.Close()
				res.Status = resp.StatusCode
				break
			}
			if attempt+1 < retries {
				time.Sleep(time.Millisecond * 100) // backoff if needed
			}
		}

		res.Latency = time.Since(reqStart)
		res.Error = err
		results <- res
	}
}

func percentile(values []float64, percentile float64) float64 {
	sort.Float64s(values)
	index := math.Max(0, math.Min(float64(len(values)-1), math.Ceil(percentile/100*float64(len(values))))-1)
	return values[int(index)]
}

func main() {
	flag.Parse()

	if url == "" {
		fmt.Println("URL is required")
		return
	}

	fmt.Printf("Sending %d HTTP %s requests to %s with concurrency %d\n", requests, method, url, concurrency)

	jobs := make(chan int, requests)
	results := make(chan Result, requests)
	var wg sync.WaitGroup

	// HTTP Client with timeout
	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			MaxIdleConnsPerHost: 100,
			MaxConnsPerHost:     100,
		},
	}

	start := time.Now()

	// Start workers
	wg.Add(concurrency)
	for i := 0; i < concurrency; i++ {
		go worker(i+1, jobs, results, client, &wg)
	}

	// Enqueue jobs
	for j := 0; j < requests; j++ {
		jobs <- j
	}
	close(jobs)

	// Collect results
	go func() {
		wg.Wait()
		close(results)
	}()

	var latencies []float64
	statusCodes := make(map[int]int)
	errors := 0

	for res := range results {
		latencies = append(latencies, res.Latency.Seconds()*1000) // in ms
		if res.Error != nil {
			errors++
		} else {
			statusCodes[res.Status]++
		}
	}

	elapsed := time.Since(start).Seconds()

	// Stats
	if len(latencies) > 0 {
		sort.Float64s(latencies)
		total := float64(len(latencies))

		fmt.Printf("\nTotal time: %.2f seconds\n", elapsed)
		fmt.Printf("Requests: %d total, %d failed, %d succeeded\n", requests, errors, requests-errors)
		fmt.Printf("Requests per second: %.2f\n", total/elapsed)
		fmt.Printf("Avg latency: %.2f ms\n", sum(latencies)/total)
		fmt.Printf("Median latency: %.2f ms\n", percentile(latencies, 50))
		fmt.Printf("P95 latency: %.2f ms\n", percentile(latencies, 95))
		fmt.Printf("P99 latency: %.2f ms\n", percentile(latencies, 99))

		fmt.Println("\nStatus codes:")
		for code, count := range statusCodes {
			fmt.Printf("  %d: %d\n", code, count)
		}
	} else {
		fmt.Println("No valid responses received.")
	}
}

func sum(arr []float64) float64 {
	s := 0.0
	for _, v := range arr {
		s += v
	}
	return s
}
