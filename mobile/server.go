package mei

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/rpc"
	"net/rpc/jsonrpc"
	"strings"
	"time"

	"github.com/caiguanhao/mei/base"
	"go.bug.st/serial"
)

type (
	server struct {
		base *base.Base
	}

	OpenArgs struct {
		Path string `json:"path"`
	}
)

func (s *server) Open(args *OpenArgs, reply *bool) (err error) {
	gen := func() (base.Port, error) {
		return serial.Open(args.Path, &serial.Mode{
			BaudRate: 9600,
			DataBits: 8,
			Parity:   serial.NoParity,
			StopBits: serial.OneStopBit,
		})
	}
	err = s.base.Open(args.Path, gen)
	*reply = err == nil
	return
}

func (s *server) Search(_ *int, reply *string) error {
	portPaths, err := serial.GetPortsList()
	if err != nil {
		return err
	}
	pathChan := make(chan string)
	for _, path := range portPaths {
		go func(path string) {
			b := base.NewBase(false)
			defer b.Close()
			if b.Open(path, func() (base.Port, error) {
				return serial.Open(path, &serial.Mode{
					BaudRate: 9600,
					DataBits: 8,
					Parity:   serial.NoParity,
					StopBits: serial.OneStopBit,
				})
			}) == nil && b.Check() == nil {
				pathChan <- path
			}
		}(path)
	}
	select {
	case path := <-pathChan:
		*reply = path
		return nil
	case <-time.After(11 * time.Second):
		return errors.New("not found")
	}
}

func StartServer(address string, verbosive bool) error {
	var s server

	s.base = base.NewBase(verbosive)
	err := rpc.RegisterName("MEI", s.base.ValueForRPCRegister())
	if err != nil {
		return err
	}

	err = rpc.RegisterName("MEISrv", &s)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", jsonrpcOverHttpHandler)
	server := &http.Server{
		Addr:    address,
		Handler: logRequest(mux),
	}
	log.Println("listening json-rpc over http", address)
	return server.ListenAndServe()
}

func jsonrpcOverHttpHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	if r.Body == nil {
		http.NotFound(w, r)
		return
	}
	defer r.Body.Close()
	res := jsonRPCRequest{r.Body, &bytes.Buffer{}}
	c := codec{codec: jsonrpc.NewServerCodec(&res)}
	rpc.ServeCodec(&c)
	w.Header().Set("Content-Type", "application/json")
	if c.isError {
		w.WriteHeader(400)
	}
	_, err := io.Copy(w, res.readWriter)
	if err != nil {
		log.Println("response error:", err)
		return
	}
	return
}

func logRequest(handler http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Println(r.Method, r.URL)
		handler.ServeHTTP(w, r)
	})
}

type (
	codec struct {
		codec   rpc.ServerCodec
		request *rpc.Request
		isError bool
	}

	jsonRPCRequest struct {
		reader     io.Reader
		readWriter io.ReadWriter
	}
)

func (c *codec) ReadRequestHeader(r *rpc.Request) error {
	c.request = r
	return c.codec.ReadRequestHeader(r)
}

func (c *codec) ReadRequestBody(x interface{}) error {
	err := c.codec.ReadRequestBody(x)
	b, _ := json.Marshal(x)
	log.Println("->", c.request.ServiceMethod, "-", strings.TrimSpace(string(b)))
	return err
}

func (c *codec) WriteResponse(r *rpc.Response, x interface{}) error {
	if r.Error == "" {
		b, _ := json.Marshal(x)
		log.Println("<-", r.ServiceMethod, "-", strings.TrimSpace(string(b)))
	} else {
		c.isError = true
		log.Println("<-", r.ServiceMethod, "-", r.Error)
	}
	return c.codec.WriteResponse(r, x)
}

func (c *codec) Close() error {
	return c.codec.Close()
}

func (r *jsonRPCRequest) Read(p []byte) (n int, err error) {
	return r.reader.Read(p)
}

func (r *jsonRPCRequest) Write(p []byte) (n int, err error) {
	return r.readWriter.Write(p)
}

func (r *jsonRPCRequest) Close() error {
	return nil
}
