package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	defaultServerTimeout = 20 * time.Second

	responseOK                        = "OK"
	responseNotFound                  = "Not found"
	responseInvalidMethod             = `Only "POST" method is supported`
	responseInvalidEncoding           = `"Content-Encoding" must be "gzip" or empty`
	responseErrGzipReader             = "Error on gzip body"
	responseErrUnmarshalBody          = "Failed to unmarshal message body"
	responseErrInternalServerError    = "Internal Server Error"
	responseErrUnsupportedMetricEvent = "Unsupported metric event"
	responseErrUnsupportedLogEvent    = "Unsupported log event"

	// Centralizing some HTTP and related string constants.
	gzipEncoding              = "gzip"
	httpContentEncodingHeader = "Content-Encoding"
)

var (
	errNilNextMetricsConsumer = errors.New("nil metricsConsumer")
	errNilNextLogsConsumer    = errors.New("nil logsConsumer")
	errEmptyEndpoint          = errors.New("empty endpoint")

	okRespBody                = initJSONResponse(responseOK)
	notFoundRespBody          = initJSONResponse(responseNotFound)
	invalidMethodRespBody     = initJSONResponse(responseInvalidMethod)
	invalidEncodingRespBody   = initJSONResponse(responseInvalidEncoding)
	errGzipReaderRespBody     = initJSONResponse(responseErrGzipReader)
	errUnmarshalBodyRespBody  = initJSONResponse(responseErrUnmarshalBody)
	errInternalServerError    = initJSONResponse(responseErrInternalServerError)
	errUnsupportedMetricEvent = initJSONResponse(responseErrUnsupportedMetricEvent)
	errUnsupportedLogEvent    = initJSONResponse(responseErrUnsupportedLogEvent)
)

type summaryData struct {
	Source         string `json:"source"`
	Eps            string `json:"eps"`
	EventsReceived int64  `json:"eventsReceived"`
	Thorughput     string `json:"thorughput"`
	DataIngestRate string `json:"dataIngestRate"`
}

type eventSourceStat struct {
	eventsReceived int64
	beginTime      time.Time
	bytesReceived  float64
	endTime        time.Time
	generatedCount int64
}

type splunkReceiver struct {
	server          *http.Server
	performanceStat map[string]*eventSourceStat
	receivedEvents  chan *Event
	eventCount      int64
	byteReceived    int64
	beginTime       time.Time
	endTime         time.Time
}

type Event struct {
	Time       *float64               `json:"time,omitempty"`
	Host       string                 `json:"host"`
	Source     string                 `json:"source,omitempty"`
	SourceType string                 `json:"sourcetype,omitempty"`
	Index      string                 `json:"index,omitempty"`
	Event      string                 `json:"event"`
	Fields     map[string]interface{} `json:"fields,omitempty"`
}

func NewLogsReceiver() (*splunkReceiver, error) {
	r := &splunkReceiver{
		performanceStat: map[string]*eventSourceStat{},
		receivedEvents:  make(chan *Event),
		eventCount:      0,
		byteReceived:    0,
		server: &http.Server{
			Addr:              ":8088",
			ReadHeaderTimeout: defaultServerTimeout,
			WriteTimeout:      defaultServerTimeout,
		},
	}

	return r, nil
}

func (r *splunkReceiver) Start() error {
	go r.consumeEvents()
	// set up the listener
	ln, err := net.Listen("tcp", ":8088")
	if err != nil {
		return fmt.Errorf("failed to bind to address %s: %w", ":8088", err)
	}

	mx := mux.NewRouter()
	mx.HandleFunc("/summary", r.summary)
	mx.NewRoute().HandlerFunc(r.receiveEvents)

	r.server = &http.Server{
		Handler: mx,
	}

	r.server.ReadHeaderTimeout = defaultServerTimeout
	r.server.WriteTimeout = defaultServerTimeout

	if errHTTP := r.server.Serve(ln); errHTTP != http.ErrServerClosed {
		log.Println("error")
		log.Fatalln(errHTTP.Error())
	}

	return err
}

func (r *splunkReceiver) receiveEvents(resp http.ResponseWriter, req *http.Request) {
	if req.ContentLength == 0 {
		resp.Write(okRespBody)
		return
	}
	dec := json.NewDecoder(req.Body)
	for dec.More() {
		var msg Event
		err := dec.Decode(&msg)
		if err != nil {
			log.Println("fail request")
			r.failRequest(resp, http.StatusBadRequest, errUnmarshalBodyRespBody)
			return
		}
		r.receivedEvents <- &msg
	}
	resp.WriteHeader(http.StatusAccepted)
	resp.Write(okRespBody)
}

func (r *splunkReceiver) consumeEvents() {
	interval := time.Second * 3
	timer := time.NewTimer(interval)
	r.beginTime = time.Now()
	for {
		select {
		case <-timer.C:
			r.endTime = time.Now()
			duration := r.endTime.Sub(r.beginTime).Seconds()
			log.Printf("EPS: %.0f\n", float64(r.eventCount)/duration)
			log.Printf("Throughput: %.0fk\n", float64(r.eventCount)/duration/1024)
			r.eventCount = 0
			r.byteReceived = 0
			r.beginTime = r.endTime
			timer.Reset(interval)
		default:
		}
		select {
		case event := <-r.receivedEvents:
			r.eventCount += 1
			r.byteReceived += int64(len(event.Event))

			if _, ok := r.performanceStat[event.Source]; !ok {
				r.performanceStat[event.Source] = &eventSourceStat{
					eventsReceived: 0,
					beginTime:      time.Now(),
					bytesReceived:  0,
					endTime:        time.Now(),
					generatedCount: 1,
				}
			}
			esStat := r.performanceStat[event.Source]
			esStat.eventsReceived += 1
			esStat.bytesReceived += float64(len(event.Event))
			esStat.endTime = time.Now()
			if event.Event[:9] == "---end---" {
				generatedCount, err := strconv.ParseInt(strings.TrimSpace(event.Event[10:]), 10, 64)
				if err != nil {
					log.Fatalln(err)
				}
				esStat.generatedCount = generatedCount
			}
		default:
		}
	}
}

func (r *splunkReceiver) calculateStats() *[]summaryData {
	var result []summaryData
	for source, stats := range r.performanceStat {
		sum := summaryData{
			Source:         source,
			Eps:            fmt.Sprintf("%0.f", float64(stats.eventsReceived)/stats.endTime.Sub(stats.beginTime).Seconds()),
			Thorughput:     fmt.Sprintf("%0.fk", stats.bytesReceived/1024),
			DataIngestRate: fmt.Sprintf("%0.f%%", float64(stats.eventsReceived)/float64(stats.generatedCount)*100),
			EventsReceived: stats.eventsReceived,
		}
		result = append(result, sum)
	}
	return &result
}

func (r *splunkReceiver) summary(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "application/json")
	result := r.calculateStats()
	js, err := json.MarshalIndent(result, "", "\t")
	if err != nil {
		http.Error(resp, err.Error(), http.StatusInternalServerError)
		return
	}
	resp.WriteHeader(http.StatusAccepted)

	_, writeErr := resp.Write(js)
	if writeErr != nil {
		log.Println("Error writing HTTP response message", zap.Error(writeErr))
	}
}

func (r *splunkReceiver) failRequest(
	resp http.ResponseWriter,
	httpStatusCode int,
	jsonResponse []byte,
) {
	resp.WriteHeader(httpStatusCode)
	if len(jsonResponse) > 0 {
		// The response needs to be written as a JSON string.
		_, writeErr := resp.Write(jsonResponse)
		if writeErr != nil {
			log.Println("Error writing HTTP response message", zap.Error(writeErr))
		}
	}
}

func initJSONResponse(s string) []byte {
	respBody, err := json.Marshal(s)
	if err != nil {
		// This is to be used in initialization so panic here is fine.
		panic(err)
	}
	return respBody
}

func main() {
	r, _ := NewLogsReceiver()
	r.Start()
}
