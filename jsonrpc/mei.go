package jsonrpc

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

var (
	ErrTimeout      = errors.New("timeout")
	ErrProcessing   = errors.New("already processing")
	ErrNoContent    = errors.New("no content")
	ErrNoSuchClient = errors.New("no such client")
)

const (
	cashChannelKey = string(0x4B)
	coinChannelKey = string(0x4C)
)

type (
	MEI struct {
		Clients *sync.Map
		Path    string

		WSClients sync.Map

		Verbosive bool

		dimesToReceive        int
		dimesReceived         int
		maxDimesForGiveChange int

		existingSession chan bool
	}

	Client interface {
		GetChannels() *sync.Map
		Write([]byte) (int, error)
	}

	BasicArgs struct {
		ClientID string `json:"client_id"`
		Retries  int    `json:"retries"`
	}

	NewSessionArgs struct {
		BasicArgs
		Seconds int `json:"seconds"`
		Dimes   int `json:"dimes"`
	}

	GiveChangeArgs struct {
		BasicArgs
		Dimes int `json:"dimes"`
	}

	status struct {
		DimesTotalReceived    int
		DimesToReceive        int
		DimesLeft             int
		MaxDimesForGiveChange int
		Completed             bool
	}

	actionMessage struct {
		Action string
	}

	receivedMessage struct {
		Action        string
		Type          string
		DimesReceived int
		status
	}

	statusMessage struct {
		Action string
		status
	}

	giveChangeMessage struct {
		Action      string
		DimesToGive int
	}

	changesGivenMessage struct {
		Action     string
		DimesGiven int
	}

	errorMessage struct {
		Action  string
		Message string
	}
)

func (args *BasicArgs) GetRetries() int {
	retries := args.Retries
	if retries < 1 || retries > 10 {
		return 10
	}
	return retries
}

func (m *MEI) EnableBillAcceptor(args *BasicArgs, reply *bool) (err error) {
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, construct(0x46)(0x01, 0xFF, 0xFF), string(0x46), 500)
		if err == nil && len(ret) > 0 && ret[0] == 0x46 {
			*reply = true
			return
		}
	}
	return
}

func (m *MEI) DisableBillAcceptor(args *BasicArgs, reply *bool) (err error) {
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, construct(0x46)(0x00, 0xFF, 0xFF), string(0x46), 500)
		if err == nil && len(ret) > 0 && ret[0] == 0x46 {
			*reply = true
			return
		}
	}
	return
}

func (m *MEI) EnableCoinAcceptor(args *BasicArgs, reply *bool) (err error) {
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, construct(0x47)(0x01, 0xFF, 0xFF), string(0x47), 500)
		if err == nil && len(ret) > 0 && ret[0] == 0x47 {
			*reply = true
			return
		}
	}
	return
}

func (m *MEI) DisableCoinAcceptor(args *BasicArgs, reply *bool) (err error) {
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, construct(0x47)(0x00, 0xFF, 0xFF), string(0x47), 500)
		if err == nil && len(ret) > 0 && ret[0] == 0x47 {
			*reply = true
			return
		}
	}
	return
}

func (m *MEI) GetMaxDimesForGiveChange(args *BasicArgs, dimes *int) (err error) {
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, construct(0x49)(0x0C), string(0x49), 500)
		if err == nil && len(ret) > 7 {
			base := bytes2int(ret[4], ret[5])
			n := 0
			for j := 0; j < int(ret[7]); j++ {
				n += base * int(ret[8+j*2]) * int(ret[9+j*2])
			}
			factor := 1
			if ret[6] == 1 {
				factor = 10
			} else if ret[6] == 2 {
				factor = 100
			}
			*dimes = n / factor * 10
			return
		}
	}
	return
}

func (m *MEI) GiveChange(args *GiveChangeArgs, reply *bool) (err error) {
	// giving changes of 300 dimes takes about 9 seconds
	timeout := args.Dimes * 40
	if timeout < 2000 {
		timeout = 2000
	} else if timeout > 20000 {
		timeout = 20000
	}
	x, y := int2bytes(args.Dimes)
	b := construct(0x54)(x, y)
	for i := 0; i < args.GetRetries(); i++ {
		var ret []byte
		ret, err = m.write(args.ClientID, b, string(0x54), timeout)
		if err == nil && len(ret) == 7 && ret[0] == 0x54 {
			*reply = true
			return
		}
	}
	return
}

func (m *MEI) NewSession(args *NewSessionArgs, reply *string) (err error) {
	if m.existingSession == nil {
		log.Println("no existing session")
	} else {
		close(m.existingSession)
	}

	client := m.getClient(args.ClientID)
	if client == nil {
		return ErrNoSuchClient
	}

	var ret bool
	log.Println("enabling bill acceptor")
	err = m.EnableBillAcceptor(&args.BasicArgs, &ret)
	if err != nil {
		return
	}
	log.Println("enabling coin acceptor")
	err = m.EnableCoinAcceptor(&args.BasicArgs, &ret)
	if err != nil {
		return
	}
	err = m.GetMaxDimesForGiveChange(&args.BasicArgs, &m.maxDimesForGiveChange)
	if err != nil {
		return
	}
	log.Println("max dimes for give change:", m.maxDimesForGiveChange)

	server := m.newServer()
	ln, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	go server.Serve(ln)
	go m.process(server, client, args)
	*reply = ln.Addr().String()

	return nil
}

func (m *MEI) write(clientId string, input []byte, channelKey string, timeout int) (output []byte, err error) {
	if len(input) == 0 {
		err = ErrNoContent
		return
	}
	client := m.getClient(clientId)
	if client == nil {
		err = ErrNoSuchClient
		return
	}
	channels := client.GetChannels()
	channel, hasChannel := channels.LoadOrStore(channelKey, make(chan []byte))
	if hasChannel {
		err = ErrProcessing
		return
	} else {
		defer channels.Delete(channelKey)
	}
	var n int
	n, err = client.Write(input)
	if m.Verbosive {
		if clientId == "" {
			log.Printf("%s: %d bytes written: % X", m.Path, n, input)
		} else {
			log.Printf("%s: %s %d bytes written: % X", m.Path, clientId, n, input)
		}
	}
	if err == nil {
		timeoutChan := newTimeoutChan(timeout)
		for {
			select {
			case output = <-channel.(chan []byte):
				return
			case <-timeoutChan:
				err = ErrTimeout
				return
			}
		}
	} else {
		log.Println("error writting", input, err)
	}
	return
}

func (m *MEI) getClient(clientId string) Client {
	if m.Clients == nil {
		return nil
	}
	_client, ok := m.Clients.Load(clientId)
	if !ok {
		return nil
	}
	client, ok := _client.(Client)
	if !ok {
		return nil
	}
	return client
}

func (m *MEI) newServer() *http.Server {
	upgrader := websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			log.Print("upgrade error:", err)
			return
		}
		defer func() {
			m.WSClients.Delete(c)
			c.Close()
		}()
		m.WSClients.Store(c, true)
		m.send(c, statusMessage{
			Action: "ready",
			status: m.currentStatus(),
		})
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseGoingAway) {
					log.Println("read error:", err)
				}
				break
			}
			log.Println("received", string(data))
		}
	})
	return &http.Server{
		Addr:    "127.0.0.1:0",
		Handler: mux,
	}
}

func (m *MEI) process(server *http.Server, client Client, args *NewSessionArgs) {
	channels := client.GetChannels()
	cashChan, _ := channels.LoadOrStore(cashChannelKey, make(chan []byte))
	coinChan, _ := channels.LoadOrStore(coinChannelKey, make(chan []byte))

	schedule := func() <-chan time.Time {
		seconds := args.Seconds
		if seconds < 10 {
			seconds = 60
		}
		log.Println("server will shut down in", seconds, "seconds")
		return time.After(time.Duration(seconds) * time.Second)
	}

	after := schedule()
	m.dimesToReceive = args.Dimes
	m.dimesReceived = 0
	m.existingSession = make(chan bool)
	defer func() {
		m.existingSession = nil
	}()
loop:
	for {
		select {
		case ret := <-coinChan.(chan []byte):
			var base float64 = 1.0
			if ret[5] == 1 {
				base = 10.0
			} else if ret[5] == 2 {
				base = 100.0
			}
			n := float64(bytes2int(ret[3], ret[4])) / base
			if m.received("coin", n) {
				break loop
			}
			after = schedule()
			m.broadcast(actionMessage{Action: "resetTimer"})
			continue loop // since "after" is updated
		case ret := <-cashChan.(chan []byte):
			var base float64 = 1.0
			if ret[6] == 1 {
				base = 10.0
			} else if ret[6] == 2 {
				base = 100.0
			}
			n := float64(bytes2int(ret[3], ret[4], ret[5])) / base
			if m.received("bill", n) {
				break loop
			}
			after = schedule()
			m.broadcast(actionMessage{Action: "resetTimer"})
			continue loop // since "after" is updated
		case <-after:
			break loop
		case <-m.existingSession:
			log.Println("aborting previous session")
			return
		}
	}

	channels.Delete(cashChannelKey)
	channels.Delete(coinChannelKey)

	m.broadcast(statusMessage{
		Action: "finished",
		status: m.currentStatus(),
	})

	changes := m.dimesReceived - m.dimesToReceive
	if changes > 0 {
		if changes > m.maxDimesForGiveChange {
			m.error(errors.New("TOO_FEW_COINS_TO_CHANGE"))
		} else {
			m.broadcast(giveChangeMessage{
				Action:      "giveChange",
				DimesToGive: changes,
			})
			var ret bool
			err := m.GiveChange(&GiveChangeArgs{
				BasicArgs: args.BasicArgs,
				Dimes:     changes,
			}, &ret)
			if err == nil {
				m.broadcast(changesGivenMessage{
					Action:     "changesGiven",
					DimesGiven: changes,
				})
			} else {
				m.error(err)
			}
		}
	}

	log.Println("disabling bill acceptor")
	var ret bool
	err := m.DisableBillAcceptor(&args.BasicArgs, &ret)
	if err != nil {
		m.error(err)
	}
	log.Println("disabling coin acceptor")
	err = m.DisableCoinAcceptor(&args.BasicArgs, &ret)
	if err != nil {
		m.error(err)
	}

	if err = server.Shutdown(context.Background()); err != nil {
		m.error(err)
	} else {
		log.Println("server has shut down")
	}
}

func (m *MEI) received(t string, n float64) bool {
	dimes := int(n * 10)
	m.dimesReceived += dimes
	log.Println("received", t, "--", n, "total dimes received:", m.dimesReceived, "dimes left:", m.dimesLeft())
	m.broadcast(receivedMessage{
		Action:        "received",
		Type:          t,
		DimesReceived: dimes,
		status:        m.currentStatus(),
	})
	return m.completed()
}

func (m *MEI) error(err error) {
	log.Println(err)
	m.broadcast(errorMessage{
		Action:  "error",
		Message: err.Error(),
	})
}

func (m *MEI) dimesLeft() int {
	left := m.dimesToReceive - m.dimesReceived
	if left < 0 {
		left = 0
	}
	return left
}

func (m *MEI) completed() bool {
	return m.dimesReceived >= m.dimesToReceive
}

func (m *MEI) currentStatus() status {
	return status{
		DimesTotalReceived:    m.dimesReceived,
		DimesToReceive:        m.dimesToReceive,
		DimesLeft:             m.dimesLeft(),
		MaxDimesForGiveChange: m.maxDimesForGiveChange,
		Completed:             m.completed(),
	}
}

func (m *MEI) send(client *websocket.Conn, content interface{}) {
	b, _ := json.Marshal(content)
	log.Println("sending to client", string(b))
	client.WriteMessage(websocket.TextMessage, b)
}

func (m *MEI) broadcast(content interface{}) {
	b, _ := json.Marshal(content)
	log.Println("broadcasting", string(b))
	m.WSClients.Range(func(conn, _ interface{}) bool {
		conn.(*websocket.Conn).WriteMessage(websocket.TextMessage, b)
		return true
	})
}

func int2bytes(i int) (x, y byte) {
	x = byte(i >> 8)
	y = byte(i)
	return
}

func bytes2int(bytes ...byte) (n int) {
	for i, l := 0, len(bytes); i < l; i++ {
		n += int(bytes[l-1-i]) << (8 * i)
	}
	return
}

func construct(function byte) func(...byte) []byte {
	return func(_data ...byte) []byte {
		data := []byte{function}
		data = append(data, byte(len(_data)))
		data = append(data, _data...)
		var sum byte
		for i := 0; i < len(data); i++ {
			sum += data[i]
		}
		data = append(data, sum)
		return data
	}
}

func newTimeoutChan(t int) <-chan time.Time {
	if t == 0 {
		t = 10000
	} else if t < 100 {
		t = 100
	}
	timeout := time.Duration(t) * time.Millisecond
	return time.After(timeout)
}
