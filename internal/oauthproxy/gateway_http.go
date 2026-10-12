package oauthproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

func requestModelID(body []byte) string {
	var payload struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &payload)
	return payload.Model
}

func copyGatewayHeaders(target, source http.Header) {
	for name, values := range source {
		if gatewayHopByHopHeader(name) {
			continue
		}
		for _, value := range values {
			target.Add(name, value)
		}
	}
}

func gatewayHopByHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func copyGatewayResponse(writer http.ResponseWriter, body io.Reader) {
	buffer := make([]byte, 32<<10)
	flusher, _ := writer.(http.Flusher)
	for {
		n, err := body.Read(buffer)
		if n > 0 {
			_, _ = writer.Write(buffer[:n])
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

func closeGatewayResponse(response *http.Response) {
	if response == nil || response.Body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 64<<10))
	_ = response.Body.Close()
}
