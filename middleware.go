package gomiddleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	PATH             = "path"
	REQUEST_HEADERS  = "requestHeaders"
	RESPONSE_HEADERS = "responseHeaders"
	METHOD           = "method"
	REQUEST_PAYLOAD  = "requestPayload"
	RESPONSE_PAYLOAD = "responsePayload"
	IP               = "ip"
	TIME             = "time"
	STATUS_CODE      = "statusCode"
	TYPE             = "type"
	STATUS           = "status"
	CONTENT_TYPE     = "contentType"
)

func Middleware(kafkaWriter *kafka.Writer) func(h http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, err := io.ReadAll(r.Body)
			defer r.Body.Close()

			if err != nil {
				log.Printf("Error reading body: %v", err)
				http.Error(w, "can't read body", http.StatusBadRequest)
				return
			}

			// And now set a new body, which will simulate the same data we read:
			r.Body = io.NopCloser(bytes.NewBuffer(body))
			cw := NewResponseWriter(w)

			next.ServeHTTP(cw, r)

			var data = make(map[string]string)
			data[PATH] = r.URL.String()
			j, err := json.Marshal(r.Header)
			data[REQUEST_HEADERS] = string(j)
			k, err := json.Marshal(cw.Header())
			data[RESPONSE_HEADERS] = string(k)
			data[METHOD] = r.Method
			data[REQUEST_PAYLOAD] = string(body)
			data[RESPONSE_PAYLOAD] = cw.Payload()

			ip := r.RemoteAddr
			xff := r.Header["x-forwarded-for"]
			if len(xff) != 0 {
				ip = xff[0]
			}
			data[IP] = ip

			data[TIME] = strconv.FormatInt(time.Now().Unix(), 10)
			data[STATUS_CODE] = strconv.Itoa(cw.Status())
			data[TYPE] = r.Proto
			data[STATUS] = "null"
			data[CONTENT_TYPE] = cw.Header().Get("Content-Type")

			message, err := json.Marshal(data)
			ctx := context.Background()
			go Produce(kafkaWriter, ctx, string(message))

		})
	}
}
