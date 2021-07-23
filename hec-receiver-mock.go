package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"log"
	"net"
	"net/http"
	"strconv"
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

type splunkReceiver struct {
	server          *http.Server
	performanceStat map[string]*eventSourceStat
}

type summaryData struct {
	Source         string    `json:"source"`
	Eps            float64   `json:"eps"`
	EventsReceived int64     `json:"eventsReceived"`
	Thorughput     float64   `json:"thorughput"`
	DataIngestRate float64   `json:"dataIngestRate"`
	BeginTime      time.Time `json:"beginTime"`
	EndTime        time.Time `json:"endTime"`
}

func NewLogsReceiver() (*splunkReceiver, error) {
	r := &splunkReceiver{
		performanceStat: map[string]*eventSourceStat{},
		server: &http.Server{
			Addr:              ":8088",
			ReadHeaderTimeout: defaultServerTimeout,
			WriteTimeout:      defaultServerTimeout,
		},
	}

	return r, nil
}

func (r *splunkReceiver) Start() error {
	// set up the listener
	ln, err := net.Listen("tcp", ":8088")
	if err != nil {
		return fmt.Errorf("failed to bind to address %s: %w", ":8088", err)
	}

	mx := mux.NewRouter()
	mx.HandleFunc("/summary", r.summary)
	mx.NewRoute().HandlerFunc(r.handleReq)

	r.server = &http.Server{
		Handler: mx,
	}

	// TODO: Evaluate what properties should be configurable, for now
	//		set some hard-coded values.
	r.server.ReadHeaderTimeout = defaultServerTimeout
	r.server.WriteTimeout = defaultServerTimeout

	if errHTTP := r.server.Serve(ln); errHTTP != http.ErrServerClosed {
		log.Println("error")
		log.Fatalln(errHTTP.Error())
	}

	return err
}

type Event struct {
	Time       *float64               `json:"time,omitempty"`       // optional epoch time - set to nil if the event timestamp is missing or unknown
	Host       string                 `json:"host"`                 // hostname
	Source     string                 `json:"source,omitempty"`     // optional description of the source of the event; typically the app's name
	SourceType string                 `json:"sourcetype,omitempty"` // optional name of a Splunk parsing configuration; this is usually inferred by Splunk
	Index      string                 `json:"index,omitempty"`      // optional name of the Splunk index to store the event in; not required if the token has a default index set in Splunk
	Event      string                 `json:"event"`                // type of event: set to "metric" or nil if the event represents a metric, or is the payload of the event.
	Fields     map[string]interface{} `json:"fields,omitempty"`     // dimensions and metric data
}

// UnmarshalJSON unmarshals the JSON representation of an event
func (e *Event) UnmarshalJSON(b []byte) error {
	rawEvent := struct {
		Time       interface{}            `json:"time,omitempty"`
		Host       string                 `json:"host"`
		Source     string                 `json:"source,omitempty"`
		SourceType string                 `json:"sourcetype,omitempty"`
		Index      string                 `json:"index,omitempty"`
		Event      string                 `json:"event"`
		Fields     map[string]interface{} `json:"fields,omitempty"`
	}{}
	err := json.Unmarshal(b, &rawEvent)
	if err != nil {
		return err
	}
	*e = Event{
		Host:       rawEvent.Host,
		Source:     rawEvent.Source,
		SourceType: rawEvent.SourceType,
		Index:      rawEvent.Index,
		Event:      rawEvent.Event,
		Fields:     rawEvent.Fields,
	}
	switch t := rawEvent.Time.(type) {
	case float64:
		e.Time = &t
	case string:
		{
			time, err := strconv.ParseFloat(t, 64)
			if err != nil {
				return err
			}
			e.Time = &time
		}
	}
	return nil
}

func (r *splunkReceiver) handleReq(resp http.ResponseWriter, req *http.Request) {
	if req.ContentLength == 0 {
		resp.Write(okRespBody)
		return
	}
	dec := json.NewDecoder(req.Body)
	var events []*Event
	for dec.More() {
		var msg Event
		err := dec.Decode(&msg)
		if err != nil {
			r.failRequest(resp, http.StatusBadRequest, errUnmarshalBodyRespBody)
			return
		}
		events = append(events, &msg)
	}
	r.consumeLogs(req.Context(), events, resp, req)
}

func (r *splunkReceiver) calculateStats() *[]summaryData {
	var result []summaryData
	for source, stats := range r.performanceStat {
		sum := summaryData{
			Source:         source,
			Eps:            float64(stats.eventsReceived) / stats.endTime.Sub(stats.beginTime).Seconds(),
			Thorughput:     stats.bytesReceived / 1024 / 1024,
			DataIngestRate: float64(stats.eventsReceived) / float64(stats.generatedCount),
			EventsReceived: stats.eventsReceived,
			BeginTime:      stats.beginTime,
			EndTime:        stats.endTime,
		}
		result = append(result, sum)
	}
	return &result
}

func (r *splunkReceiver) summary(resp http.ResponseWriter, req *http.Request) {
	resp.Header().Set("Content-Type", "application/json")
	result := r.calculateStats()
	js, err := json.Marshal(result)
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

type eventSourceStat struct {
	eventsReceived int64
	beginTime      time.Time
	bytesReceived  float64
	endTime        time.Time
	generatedCount int64
}

func (r *splunkReceiver) consumeLogs(ctx context.Context, events []*Event, resp http.ResponseWriter, req *http.Request) {
	for _, event := range events {
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
			generatedCount, err := strconv.ParseInt(event.Event[10:], 10, 64)
			if err != nil {
				log.Fatalln(err)
			}
			esStat.generatedCount = generatedCount
		}
	}

	resp.WriteHeader(http.StatusAccepted)
	resp.Write(okRespBody)
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
	log.Println("start")
	r.Start()
}
