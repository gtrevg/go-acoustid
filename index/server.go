package index

import (
	"net/http"
	"encoding/json"
	"strconv"
	"log"
)

type context struct {
	idx *Index
}

type indexHandler struct { context }
type statsHandler struct { context }

func (h *indexHandler) ServeGET(w http.ResponseWriter, r *http.Request) {
	type Response struct {
		Status string `json:"status"`
	}
	response := &Response{ Status: "ok" }
	writeResponse(w, http.StatusOK, response)
}

func (h *indexHandler) ServePOST(w http.ResponseWriter, r *http.Request) {
	type Doc struct {
		ID uint32 `json:"id"`
		Hashes []uint32 `json:"hashes"`
	}
	type Request struct {
		Docs []Doc `json:"docs"`
	}

	decoder := json.NewDecoder(r.Body)

	var req Request
	err := decoder.Decode(&req)
	if err != nil {
		writeErrorResponse(w, http.StatusBadRequest, "invalid request body")
		return
	}

	if len(req.Docs) == 0 {
		writeErrorResponse(w, http.StatusBadRequest, "no docs")
		return
	}

	for _, doc := range req.Docs {
		if doc.ID == 0 {
			writeErrorResponse(w, http.StatusBadRequest, "missing ID")
			return
		}
		if len(doc.Hashes) == 0 {
			writeErrorResponse(w, http.StatusBadRequest, "missing hashes")
			return
		}
	}

	for _, doc := range req.Docs {
		h.idx.Add(doc.ID, doc.Hashes)
	}

	type Response struct {
		Status string `json:"status"`
	}
	response := &Response{ Status: "ok" }
	writeResponse(w, http.StatusOK, response)
}

func (h *indexHandler) ServeDELETE(w http.ResponseWriter, r *http.Request) {
	type Response struct {
		Status string `json:"status"`
	}
	response := &Response{ Status: "ok" }
	writeResponse(w, http.StatusOK, response)
}

func (h *indexHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		h.ServeGET(w, r)
	case "POST":
		h.ServePOST(w, r)
	case "DELETE":
		h.ServeDELETE(w, r)
	default:
		writeErrorResponse(w, http.StatusMethodNotAllowed, "only methods GET, POST and DELETE are allowed")
	}
}

func (h *statsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	type Response struct {
		NumDocs int `json:"num_docs"`
		NumSegments int `json:"num_segments"`
	}
	response := &Response{}
	writeResponse(w, http.StatusOK, response)
}

func writeResponse(w http.ResponseWriter, status int, response interface{}) {
	body, err := json.Marshal(response)
	if err != nil {
		log.Printf("error while serializing JSON response (%v)", err)
		writeErrorResponse(w, http.StatusInternalServerError, "JSON serialization error")
		return
	}
	body = append(body, '\n')
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	w.Write(body)
}

func writeErrorResponse(w http.ResponseWriter, status int, message string) {
	response := map[string]string{ "message": message }
	writeResponse(w, status, response)
}

func ListenAndServe(addr string, idx *Index) error {
	context := context{ idx: idx }
	mux := http.NewServeMux()
	mux.Handle("/index", &indexHandler{ context: context })
	mux.Handle("/stats", &statsHandler{ context: context })
	return http.ListenAndServe(addr, mux)
}