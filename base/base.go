package base

import (
	"errors"
	"log"
	"sync"
	"time"

	"github.com/caiguanhao/mei/jsonrpc"
)

type (
	Base struct {
		port      Port
		portPath  string
		jsonrpc   *jsonrpc.MEI
		channels  *sync.Map
		verbosive bool
	}

	Port interface {
		Write(p []byte) (n int, err error)
		Read(p []byte) (n int, err error)
		Close() error
	}

	BasicArgs      = jsonrpc.BasicArgs
	NewSessionArgs = jsonrpc.NewSessionArgs
	GiveChangeArgs = jsonrpc.GiveChangeArgs
)

func NewBase(verbosive bool) (b *Base) {
	var clients sync.Map
	var channels sync.Map
	b = &Base{
		jsonrpc: &jsonrpc.MEI{
			Clients:   &clients,
			Verbosive: verbosive,
		},
		channels: &channels,
	}
	clients.Store("", b)
	return
}

func (b *Base) Open(path string, generator func() (Port, error)) (err error) {
	b.port, err = generator()
	if err != nil {
		log.Println("error opening port", path, err)
		b.port = nil
	}
	if b.port == nil {
		return
	}
	b.portPath = path
	b.jsonrpc.Path = path
	go b.read()
	return
}

func (b *Base) GetChannels() *sync.Map {
	return b.channels
}

func (b *Base) Write(input []byte) (int, error) {
	if b.port == nil {
		return 0, errors.New("port not found")
	}
	return b.port.Write(input)
}

func (b *Base) Close() error {
	if b.port == nil {
		return errors.New("port not found")
	}
	err := b.port.Close()
	b.port = nil
	return err
}

func (b *Base) Check() error {
	var reply int
	return b.jsonrpc.GetMaxDimesForGiveChange(&BasicArgs{}, &reply)
}

func (b *Base) ValueForRPCRegister() interface{} {
	return b.jsonrpc
}

func (b *Base) read() {
	buf := make([]byte, 20)
	data := []byte{}
	showOpened := false
	for {
		if b.port == nil {
			return
		}

		n, err := b.port.Read(buf)
		if err != nil {
			log.Println(err)
			time.Sleep(2 * time.Second)
			continue
		}

		if n == 0 {
			log.Println("EOF")
			time.Sleep(2 * time.Second)
			continue
		}

		if !showOpened {
			log.Println("successfully opened port")
			showOpened = true
		}

		buffer := buf[:n]
		data = append(data, buffer...)
		data = b.process(data, b.GetChannels())
	}
}

func (b *Base) process(_data []byte, multiChannels ...*sync.Map) []byte {
	if b.verbosive {
		log.Println("processing", _data)
	}
	for i := 0; i < len(_data)-1; i++ {
		dataSize := int(_data[i+1])
		size := 3 + dataSize
		if i+size > len(_data) {
			continue
		}
		data := _data[i : i+size]
		if !validData(data) {
			continue
		}
		_data = _data[i+size:]
		i = -1 // i will be reset to 0 after this loop
		if b.verbosive {
			log.Println("received", data)
		}
		for _, channels := range multiChannels {
			if data[0] == 0x55 { // this consider error
				channels.Range(func(_, channel interface{}) bool {
					channel.(chan []byte) <- data
					return true
				})
			} else if channel, ok := channels.Load(string(data[0])); ok {
				// accepts only FINISH message for give-change
				if data[0] == 0x54 && len(data) != 7 {
					continue
				}
				channel.(chan []byte) <- data
			}
		}
	}
	return _data
}

func validData(input []byte) bool {
	if len(input) < 2 {
		return false
	}
	var sum byte
	for i := 0; i < len(input)-1; i++ {
		sum += input[i]
	}
	return sum&0xff == input[len(input)-1]
}
