package main

/*
 * Product API, implementing the Product section of api.yaml.
 *
 *   GET  /products/:productId          -> 200, 404, 500
 *   POST /products/:productId/details  -> 204, 400, 404, 500
 *   GET  /metrics                      -> Prometheus exposition format
 *   GET  /health                       -> 200  (not in the spec, used by ECS)
 *
 * Two decisions worth knowing about, both explained in the README:
 *
 * 1. The spec has no endpoint that creates a product, yet POST can return
 *    404 "Product not found". Taken literally nothing could ever exist and
 *    every POST would 404 forever. So POST is treated as update-only and the
 *    store is seeded at startup. That is the only reading where 404 on POST
 *    is reachable, which the assignment requires.
 *
 * 2. Every Product field is a pointer. weight has minimum 0, and 0 is also
 *    Go's zero value for an int, so a plain int32 cannot tell "weight was 0"
 *    apart from "weight was never sent". A nil pointer means absent.
 *
 * Observability: one middleware records the three RED signals as Prometheus
 * metrics and writes one JSON log line per request. Both come from the same
 * measurement, so the dashboard and the logs can never disagree.
 */

import (
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// ///////////////////////// TYPES /////////////////////////

// Product mirrors the Product schema in api.yaml. The json tags matter:
// the spec uses snake_case and Go uses CamelCase, so without them the
// contract breaks.
type Product struct {
	ProductID    *int32  `json:"product_id"`
	SKU          *string `json:"sku"`
	Manufacturer *string `json:"manufacturer"`
	CategoryID   *int32  `json:"category_id"`
	Weight       *int32  `json:"weight"`
	SomeOtherID  *int32  `json:"some_other_id"`
}

// APIError mirrors the Error schema. details is optional in the spec, so
// omitempty leaves it out when it is empty.
type APIError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
	Details string `json:"details,omitempty"`
}

// ///////////////////////// METRICS /////////////////////////

// The three RED signals.
//
// Every metric is labelled by route rather than by path. c.FullPath() gives
// the route template, "/products/:productId", so a million different product
// ids collapse into one time series. Labelling by the raw path would create a
// new series per id and eventually fall over, which is the classic way people
// break a Prometheus server.
var (
	// R, rate. Also carries the status label, so errors can be derived
	// from it as well.
	requestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests, by method, route and status code.",
	}, []string{"method", "route", "status"})

	// E, errors. Redundant with the above, kept as its own counter so the
	// error panel is a simple query rather than a filtered one.
	errorsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_request_errors_total",
		Help: "HTTP requests that returned a non 2xx status code.",
	}, []string{"method", "route", "status"})

	// D, duration. A histogram, not a gauge or a summary, because quantiles
	// have to be computable across all instances at query time.
	//
	// The default buckets start at 5ms. These handlers finish in tens of
	// microseconds, so every request would land in the first bucket and
	// every quantile would be meaningless. These buckets run from 100
	// microseconds up to about 0.8 seconds instead.
	requestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Request duration in seconds, by method and route.",
		Buckets: prometheus.ExponentialBuckets(0.0001, 2, 14),
	}, []string{"method", "route"})
)

// ///////////////////////// LOGGING /////////////////////////

// One structured line per request. The same three signals as the metrics,
// recoverable after the fact from a log file when nobody had a dashboard open.
type requestLog struct {
	Time       string  `json:"time"`
	Method     string  `json:"method"`
	Route      string  `json:"route"`
	Path       string  `json:"path"`
	Status     int     `json:"status"`
	DurationMs float64 `json:"duration_ms"`
	ClientIP   string  `json:"client_ip"`
}

func writeLog(entry requestLog) {
	encoded, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// A single Write of a whole line, rather than an Encoder, so that
	// concurrent requests cannot interleave halfway through a line.
	os.Stdout.Write(append(encoded, '\n'))
}

// ///////////////////////// MIDDLEWARE /////////////////////////

// Runs for every request. c.Next() hands control to the actual handler and
// returns once it has finished, so everything before it is "before the
// request" and everything after it is "after the response".
func observability() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()

		c.Next()

		elapsed := time.Since(start)
		status := c.Writer.Status()

		// FullPath is the route template. It is empty for a request that
		// matched no route at all, which would otherwise produce an empty
		// label.
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}

		statusText := strconv.Itoa(status)
		method := c.Request.Method

		requestsTotal.WithLabelValues(method, route, statusText).Inc()
		requestDuration.WithLabelValues(method, route).Observe(elapsed.Seconds())
		if status >= 400 {
			errorsTotal.WithLabelValues(method, route, statusText).Inc()
		}

		writeLog(requestLog{
			Time:       start.UTC().Format(time.RFC3339Nano),
			Method:     method,
			Route:      route,
			Path:       c.Request.URL.Path,
			Status:     status,
			DurationMs: float64(elapsed.Microseconds()) / 1000.0,
			ClientIP:   c.ClientIP(),
		})
	}
}

// ///////////////////////// STORE /////////////////////////

// A map keyed by product id gives O(1) lookup, which is what GET needs.
// The RWMutex is here because every Gin handler runs in its own goroutine,
// so without it concurrent requests would be a data race on the map.
var (
	mu    sync.RWMutex
	store = make(map[int32]Product)
)

func seedStore() {
	seeds := []Product{
		{i32(1), str("ABC-123-XYZ"), str("Acme Corporation"), i32(456), i32(1250), i32(789)},
		{i32(2), str("DEF-456-UVW"), str("Globex"), i32(457), i32(0), i32(790)},
		{i32(3), str("GHI-789-RST"), str("Initech"), i32(458), i32(3400), i32(791)},
	}
	for _, p := range seeds {
		store[*p.ProductID] = p
	}
}

// ///////////////////////// VALIDATION /////////////////////////

// Checks the body against every rule in the Product schema. Returns a
// human readable reason and false when something is wrong.
func validate(p Product) (string, bool) {
	if p.ProductID == nil {
		return "product_id is required", false
	}
	if *p.ProductID < 1 {
		return "product_id must be at least 1", false
	}
	if p.SKU == nil {
		return "sku is required", false
	}
	if len(*p.SKU) < 1 || len(*p.SKU) > 100 {
		return "sku must be between 1 and 100 characters", false
	}
	if p.Manufacturer == nil {
		return "manufacturer is required", false
	}
	if len(*p.Manufacturer) < 1 || len(*p.Manufacturer) > 200 {
		return "manufacturer must be between 1 and 200 characters", false
	}
	if p.CategoryID == nil {
		return "category_id is required", false
	}
	if *p.CategoryID < 1 {
		return "category_id must be at least 1", false
	}
	if p.Weight == nil {
		return "weight is required", false
	}
	if *p.Weight < 0 {
		return "weight must be at least 0", false
	}
	if p.SomeOtherID == nil {
		return "some_other_id is required", false
	}
	if *p.SomeOtherID < 1 {
		return "some_other_id must be at least 1", false
	}
	return "", true
}

// ///////////////////////// HANDLERS /////////////////////////

// GET /products/:productId
//
// The spec lists no 400 for this endpoint, only 200, 404 and 500. So an id
// that is not a positive integer is reported as 404 rather than 400.
func getProduct(c *gin.Context) {
	id, ok := parseID(c.Param("productId"))
	if !ok {
		c.JSON(http.StatusNotFound, APIError{
			Error:   "NOT_FOUND",
			Message: "Product not found",
			Details: "Product ID must be a positive integer",
		})
		return
	}

	mu.RLock()
	product, found := store[id]
	mu.RUnlock()

	if !found {
		c.JSON(http.StatusNotFound, APIError{
			Error:   "NOT_FOUND",
			Message: "Product not found",
			Details: "No product with ID " + strconv.Itoa(int(id)),
		})
		return
	}

	// Deliberate 500 hook so the error path can be demonstrated. Documented
	// in the README as a test hook, not organic behaviour.
	if *product.SKU == "FORCE_500" {
		c.JSON(http.StatusInternalServerError, APIError{
			Error:   "INTERNAL_ERROR",
			Message: "Internal server error",
			Details: "Triggered deliberately by the FORCE_500 test hook",
		})
		return
	}

	c.JSON(http.StatusOK, product)
}

// POST /products/:productId/details
//
// Returns 204 with no body on success, which is what the spec asks for.
func addProductDetails(c *gin.Context) {
	// This endpoint does define a 400, so a bad path id is a 400 here even
	// though the same mistake is a 404 on GET.
	id, ok := parseID(c.Param("productId"))
	if !ok {
		c.JSON(http.StatusBadRequest, APIError{
			Error:   "INVALID_INPUT",
			Message: "The provided input data is invalid",
			Details: "Product ID must be a positive integer",
		})
		return
	}

	var product Product
	if err := c.ShouldBindJSON(&product); err != nil {
		c.JSON(http.StatusBadRequest, APIError{
			Error:   "INVALID_INPUT",
			Message: "The provided input data is invalid",
			Details: "Request body is not valid JSON: " + err.Error(),
		})
		return
	}

	if reason, valid := validate(product); !valid {
		c.JSON(http.StatusBadRequest, APIError{
			Error:   "INVALID_INPUT",
			Message: "The provided input data is invalid",
			Details: reason,
		})
		return
	}

	// The spec does not say whether the id in the body must match the id in
	// the path. Letting them disagree would allow a write to one id to carry
	// a different id inside it, so a mismatch is rejected.
	if *product.ProductID != id {
		c.JSON(http.StatusBadRequest, APIError{
			Error:   "INVALID_INPUT",
			Message: "The provided input data is invalid",
			Details: "product_id in the body does not match the ID in the path",
		})
		return
	}

	// Same deliberate 500 hook as above.
	if *product.SKU == "FORCE_500" {
		c.JSON(http.StatusInternalServerError, APIError{
			Error:   "INTERNAL_ERROR",
			Message: "Internal server error",
			Details: "Triggered deliberately by the FORCE_500 test hook",
		})
		return
	}

	mu.Lock()
	_, found := store[id]
	if found {
		store[id] = product
	}
	mu.Unlock()

	if !found {
		c.JSON(http.StatusNotFound, APIError{
			Error:   "NOT_FOUND",
			Message: "Product not found",
			Details: "No product with ID " + strconv.Itoa(int(id)),
		})
		return
	}

	c.Status(http.StatusNoContent)
}

func health(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// ///////////////////////// HELPERS /////////////////////////

// The spec says productId is an int32 with minimum 1, so anything that is
// not a positive 32 bit integer is rejected here.
func parseID(raw string) (int32, bool) {
	parsed, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || parsed < 1 {
		return 0, false
	}
	return int32(parsed), true
}

func i32(v int32) *int32   { return &v }
func str(v string) *string { return &v }

// ///////////////////////// MAIN /////////////////////////

func main() {
	seedStore()

	// gin.New rather than gin.Default, because Default installs Gin's own
	// plain text request logger and this server emits JSON instead. Recovery
	// is still wanted, so it is added back by hand.
	router := gin.New()
	router.Use(gin.Recovery())
	router.Use(observability())

	router.GET("/health", health)
	router.GET("/metrics", gin.WrapH(promhttp.Handler()))
	router.GET("/products/:productId", getProduct)
	router.POST("/products/:productId/details", addProductDetails)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	router.Run(":" + port)
}
