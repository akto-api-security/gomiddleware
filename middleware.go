package gomiddleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/segmentio/kafka-go"
)

const (
	const_akto_account_id  = "akto_account_id"
	const_path             = "path"
	const_request_headers  = "requestHeaders"
	const_response_headers = "responseHeaders"
	const_method           = "method"
	const_request_payload  = "requestPayload"
	const_response_payload = "responsePayload"
	const_ip               = "ip"
	const_time             = "time"
	const_status_code      = "statusCode"
	const_type             = "type"
	const_status           = "status"
	const_content_type     = "contentType"
)

func Middleware(kafkaWriter *kafka.Writer, akto_account_id int) func(h http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			body, err := ioutil.ReadAll(r.Body)
			defer r.Body.Close()

			if err != nil {
				log.Printf("Error reading body: %v", err)
				http.Error(w, "can't read body", http.StatusBadRequest)
				return
			}

			// And now set a new body, which will simulate the same data we read:
			r.Body = ioutil.NopCloser(bytes.NewBuffer(body))
			cw := NewResponseWriter(w)

			next.ServeHTTP(cw, r)

			dl := doLogBasedOnResponseHeader(cw.Header())
			if dl {
				process(akto_account_id, r, cw, kafkaWriter, body)
			}

		})
	}
}

func process(akto_account_id int, r *http.Request, cw ResponseWriter, kafkaWriter *kafka.Writer, body []byte) {
	var data = make(map[string]string)

	data[const_akto_account_id] = strconv.Itoa(akto_account_id)
	data[const_path] = r.URL.Path
	j, err := json.Marshal(r.Header)
	if err != nil {
		return
	}

	data[const_request_headers] = string(j)
	k, err := json.Marshal(cw.Header())
	if err != nil {
		return
	}
	data[const_response_headers] = string(k)
	data[const_method] = r.Method
	data[const_request_payload] = string(body)
	data[const_response_payload] = cw.Payload()

	ip := r.RemoteAddr
	xff := r.Header["x-forwarded-for"]
	if len(xff) != 0 {
		ip = xff[0]
	}
	data[const_ip] = ip

	data[const_time] = strconv.FormatInt(time.Now().Unix(), 10)
	data[const_status_code] = strconv.Itoa(cw.Status())
	data[const_type] = r.Proto
	data[const_status] = "null"
	data[const_content_type] = cw.Header().Get("Content-Type")

	message, err := json.Marshal(data)
	if err != nil {
		return
	}
	ctx := context.Background()
	go Produce(kafkaWriter, ctx, string(message))

}
