package main

import (
	"fmt"
	"github.com/ochinchina/supervisord/logger"
	"net/http"

	"github.com/gorilla/mux"
)

// Logtail tails the process log through http interface
type Logtail struct {
	router     *mux.Router
	supervisor *Supervisor
}

// NewLogtail creates a Logtail object
func NewLogtail(supervisor *Supervisor) *Logtail {
	return &Logtail{router: mux.NewRouter(), supervisor: supervisor}
}

// CreateHandler creates http handlers to process the program stdout and stderr through http interface
func (lt *Logtail) CreateHandler() http.Handler {
	lt.router.HandleFunc("/logtail/{program}/stdout", lt.getStdoutLog).Methods("GET")
	lt.router.HandleFunc("/logtail/{program}/stderr", lt.getStderrLog).Methods("GET")
	return lt.router
}

func (lt *Logtail) getStdoutLog(w http.ResponseWriter, req *http.Request) {
	lt.getLog("stdout", w, req)
}

func (lt *Logtail) getStderrLog(w http.ResponseWriter, req *http.Request) {
	lt.getLog("stderr", w, req)
}

func (lt *Logtail) getLog(logType string, w http.ResponseWriter, req *http.Request) {
	vars := mux.Vars(req)
	program := vars["program"]
	procMgr := lt.supervisor.GetManager()
	proc := procMgr.Find(program)

	if proc == nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	var ok bool = false
	var compositeLogger *logger.CompositeLogger = nil
	if logType == "stdout" {
		compositeLogger, ok = proc.StdoutLog.(*logger.CompositeLogger)
	} else {
		compositeLogger, ok = proc.StderrLog.(*logger.CompositeLogger)
	}

	if !ok {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	s, err := compositeLogger.ReadLog(0, 0)
	if err != nil {
		fmt.Println(err)
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(err.Error() + "\n"))
		return
	}
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(s)); err != nil {
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	ch := make(chan []byte, 100)
	chanLogger := logger.NewChanLogger(ch)
	compositeLogger.AddLogger(chanLogger)
	defer func() {
		compositeLogger.RemoveLogger(chanLogger)
		_ = chanLogger.Close()
	}()
	for {
		text, more := <-ch
		if !more {
			return
		}
		if _, err := w.Write(text); err != nil {
			return
		}
		flusher.Flush()
	}
}
