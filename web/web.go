package web

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"brooce/config"
	myredis "brooce/redis"
)

var redisClient = myredis.Get()
var redisHeader = config.Config.ClusterName
var redisLogHeader = config.Config.ClusterLogName

var reqHandler = http.NewServeMux()
var templates = makeTemplate()

var serv = &http.Server{
	Addr:         config.Config.Web.Addr,
	Handler:      reqHandler,
	ReadTimeout:  10 * time.Second,
	WriteTimeout: 10 * time.Second,
}

var webLogWriter *os.File

type httpReply struct {
	*bytes.Buffer
	statusCode int
	redirect   string
}

type httpHandler func(*http.Request, *httpReply) error
type middleware func(httpHandler) httpHandler

func Start() {
	if config.Config.Web.Disable {
		log.Println("Web interface disabled!")
		return
	}

	basePath := config.Config.Web.BasePath

	reqHandler.HandleFunc(basePath+"/up", makeHealthcheckHandler())
	reqHandler.HandleFunc(basePath+"/", makeHandler(mainpageHandler, "GET"))

	reqHandler.HandleFunc(basePath+"/failed/", makeHandler(joblistHandler, "GET"))
	reqHandler.HandleFunc(basePath+"/done/", makeHandler(joblistHandler, "GET"))
	reqHandler.HandleFunc(basePath+"/delayed/", makeHandler(joblistHandler, "GET"))
	reqHandler.HandleFunc(basePath+"/pending/", makeHandler(joblistHandler, "GET"))

	reqHandler.HandleFunc(basePath+"/search", makeHandler(searchHandler, "GET"))

	reqHandler.HandleFunc(basePath+"/retry/failed/", makeHandler(retryHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/retry/done/", makeHandler(retryHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/retry/delayed/", makeHandler(retryHandler, "POST"))

	reqHandler.HandleFunc(basePath+"/retryall/failed/", makeHandler(retryAllHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/retryall/delayed/", makeHandler(retryAllHandler, "POST"))

	reqHandler.HandleFunc(basePath+"/delete/failed/", makeHandler(deleteHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/delete/done/", makeHandler(deleteHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/delete/delayed/", makeHandler(deleteHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/delete/pending/", makeHandler(deleteHandler, "POST"))

	reqHandler.HandleFunc(basePath+"/deleteall/failed/", makeHandler(deleteAllHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/deleteall/done/", makeHandler(deleteAllHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/deleteall/delayed/", makeHandler(deleteAllHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/deleteall/pending/", makeHandler(deleteAllHandler, "POST"))

	reqHandler.HandleFunc(basePath+"/showlog/", makeHandler(showlogHandler, "GET"))

	reqHandler.HandleFunc(basePath+"/cron", makeHandler(cronpageHandler, "GET"))
	//reqHandler.HandleFunc(basePath + "/savecron", makeHandler(saveCronHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/deletecron", makeHandler(deleteCronHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/disablecron", makeHandler(disableCronHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/enablecron", makeHandler(enableCronHandler, "POST"))
	reqHandler.HandleFunc(basePath+"/schedulecron", makeHandler(scheduleCronHandler, "POST"))

	go func() {
		var err error

		if !config.Config.Web.NoLog {
			filename := filepath.Join(config.BrooceLogDir, "web.log")
			webLogWriter, err = os.OpenFile(filename, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
			if err != nil {
				log.Fatalln("Unable to open logfile", filename, "for writing! Error was", err)
			}
			defer webLogWriter.Close()
		}

		if config.Config.Web.CertFile == "" && config.Config.Web.KeyFile == "" {
			announceHttpStart("HTTP", config.Config.Web.Addr, basePath)
			err = serv.ListenAndServe()
		} else {
			announceHttpStart("HTTPS", config.Config.Web.Addr, basePath)
			err = serv.ListenAndServeTLS(config.Config.Web.CertFile, config.Config.Web.KeyFile)
		}

		if err != nil {
			log.Println("Warning: Unable to start web server, error was:", err)
		}
	}()
}

func announceHttpStart(serverType, address, basePath string) {
	if basePath != "" {
		log.Println("Starting", serverType, "server on", address, "with basepath", basePath)
	} else {
		log.Println("Starting", serverType, "server on", address)
	}
}

func makeHealthcheckHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	}
}

func makeHandler(fn httpHandler, method string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {

		rep := &httpReply{
			Buffer:     &bytes.Buffer{},
			statusCode: http.StatusOK,
		}

		var err error
		if method == req.Method {
			err = allMiddleware(fn)(req, rep)
		} else {
			rep.statusCode = http.StatusNotFound
		}

		if req.Method != "GET" && rep.statusCode == http.StatusOK && rep.Len() == 0 {
			rep.statusCode = http.StatusSeeOther
		}

		repLength := rep.Len()

		switch rep.statusCode {
		case http.StatusInternalServerError:
			http.Error(w, "500 Internal Server Error\n", rep.statusCode)
		case http.StatusNotFound:
			http.Error(w, "404 File Not Found\n", rep.statusCode)
		case http.StatusForbidden:
			http.Error(w, "403 Forbidden\n", rep.statusCode)
		case http.StatusUnauthorized:
			w.Header().Set("WWW-Authenticate", `Basic realm="brooce"`)
			http.Error(w, "401 Password Required\n", rep.statusCode)
		case http.StatusSeeOther:
			if rep.redirect == "" {
				rep.redirect = req.Referer()
			}
			http.Redirect(w, req, rep.redirect, http.StatusSeeOther)
		default:
			io.Copy(w, rep)
		}

		if webLogWriter != nil {
			logLine := fmt.Sprintf(`%s - [%s] "%s %s" %d %d "%s" "%s"`,
				req.RemoteAddr[:strings.LastIndex(req.RemoteAddr, ":")],
				time.Now().Format("02/Jan/2006:15:04:05 -0700"),
				req.Method,
				req.RequestURI,
				rep.statusCode,
				repLength,
				req.Referer(),
				req.UserAgent(),
			)

			webLogWriter.Write([]byte(logLine + "\n"))
			if err != nil {
				webLogWriter.Write([]byte(fmt.Sprintf("ERROR IN LAST REQUEST: %v\n", err)))
			}
		}

		if err != nil {
			log.Println("[web] Error:", err)
		}

	}
}

func allMiddleware(fn httpHandler) httpHandler {
	// these middlewares will be run in reverse!
	middlewares := []middleware{
		csrfMiddleware,
		authMiddleware,
		errorMiddleware,
	}

	allMiddlewareFn := fn

	for _, middlewareFunc := range middlewares {
		allMiddlewareFn = middlewareFunc(allMiddlewareFn)
	}
	return allMiddlewareFn
}

func authMiddleware(next httpHandler) httpHandler {
	return func(req *http.Request, rep *httpReply) (err error) {

		if config.Config.Web.Username != "" && config.Config.Web.Password != "" {
			username, password, authOk := req.BasicAuth()
			if !authOk || username != config.Config.Web.Username || password != config.Config.Web.Password {
				rep.statusCode = http.StatusUnauthorized
				return
			}
		}

		err = next(req, rep)
		return
	}
}

func errorMiddleware(next httpHandler) httpHandler {
	return func(req *http.Request, rep *httpReply) (err error) {

		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("Panic: %v", r)
				rep.statusCode = http.StatusInternalServerError
				return
			}
		}()

		err = next(req, rep)

		if err != nil {
			rep.statusCode = http.StatusInternalServerError
		}

		return
	}
}

func csrfMiddleware(next httpHandler) httpHandler {
	return func(req *http.Request, rep *httpReply) (err error) {

		if req.Method != "GET" && req.FormValue("csrf") != config.Config.CSRF() {
			rep.statusCode = http.StatusForbidden
			return
		}

		err = next(req, rep)
		return
	}
}

func splitUrlPath(path string) []string {
	return strings.Split(strings.Trim(strings.TrimPrefix(path, config.Config.Web.BasePath), "/"), "/")
}
