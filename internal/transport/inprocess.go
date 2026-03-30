package transport

import (
	"net/http"
	"net/http/httptest"
)

func NewHandlerClient(handler http.Handler) *http.Client {
	return &http.Client{
		Transport: handlerRoundTripper{handler: handler},
	}
}

type handlerRoundTripper struct {
	handler http.Handler
}

func (t handlerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rr := httptest.NewRecorder()
	clone := req.Clone(req.Context())
	clone.Body = req.Body
	clone.RequestURI = req.URL.RequestURI()
	t.handler.ServeHTTP(rr, clone)
	return rr.Result(), nil
}
