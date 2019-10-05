/*
The MIT License (MIT)

Copyright (c) 2016 winlin

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

/*
 This the main entrance of rtmplb, load-balance for rtmp streaming.
*/
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	oh "github.com/ossrs/go-oryx-lib/http"
	oj "github.com/ossrs/go-oryx-lib/json"
	ol "github.com/ossrs/go-oryx-lib/logger"
	oo "github.com/ossrs/go-oryx-lib/options"
	"io"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

var signature = fmt.Sprintf("RTMPLB/%v", Version())

// The config object for rtmplb module.
type RtmpLbConfig struct {
	Config
	Rtmp struct {
		Listen       string   `json:"listen"`
		Backend      []string `json:"backend"`
		UseRtmpProxy bool     `json:"proxy"`
	} `json:"rtmp"`
}

func (v *RtmpLbConfig) String() string {
	return fmt.Sprintf("%v, listen=%v, backend=%v, proxy=%v",
		&v.Config, v.Rtmp.Listen, v.Rtmp.Backend, v.Rtmp.UseRtmpProxy)
}

func (v *RtmpLbConfig) Loads(c string) (err error) {
	var f *os.File
	if f, err = os.Open(c); err != nil {
		ol.E(nil, "Open config failed, err is", err)
		return
	}
	defer f.Close()

	r := json.NewDecoder(oj.NewJsonPlusReader(f))
	if err = r.Decode(v); err != nil {
		ol.E(nil, "Decode config failed, err is", err)
		return
	}

	if err = v.Config.OpenLogger(); err != nil {
		ol.E(nil, "Open logger failed, err is", err)
		return
	}

	if len(v.Rtmp.Listen) == 0 {
		return fmt.Errorf("No rtmp listens")
	}
	if nn := strings.Count(v.Rtmp.Listen, "://"); nn != 1 {
		return fmt.Errorf("Listen %v contains %v network", v.Rtmp.Listen, nn)
	}

	if len(v.Rtmp.Backend) == 0 {
		return errors.New("no backend")
	}
	for index, backend := range v.Rtmp.Backend {
		if nn := strings.Count(backend, "://"); nn != 1 {
			return fmt.Errorf("Backend %v %v contains %v network", index, backend, nn)
		}
	}

	return
}

// The tcp porxy for rtmp backend.
type proxy struct {
	conf    *RtmpLbConfig
	lbIndex uint
}

func NewProxy(conf *RtmpLbConfig) *proxy {
	return &proxy{conf: conf}
}

const (
	// when backend connect error, retry interval.
	RetryBackend = time.Duration(3) * time.Second
	// when backend connect error, retry max count.
	RetryMax = 3
)

func (v *proxy) serveRtmp(ctx context.Context, client *net.TCPConn) (err error) {
	defer func() {
		if r := recover(); r != nil {
			if err == nil {
				err = fmt.Errorf("panic %v", r)
				ol.W(ctx, "ignore panic, err is", err)
			} else {
				ol.W(ctx, fmt.Sprintf("ignore panic %v, err is %v", r, err))
			}
		}
	}()
	defer client.Close()

	// connect to backend.
	var backend *net.TCPConn
	connectBackend := func() error {
		defer func() {
			if backend == nil {
				time.Sleep(RetryBackend)
			}
		}()

		var proto, addr string
		if backend := v.conf.Rtmp.Backend[v.lbIndex]; backend != "" {
			v.lbIndex = (v.lbIndex + 1) % uint(len(v.conf.Rtmp.Backend))
			addrs := strings.Split(backend, "://")
			proto, addr = addrs[0], addrs[1]
		}

		if c, err := net.DialTimeout(proto, addr, RetryBackend); err != nil {
			ol.W(ctx, "connect backend", addr, "failed, err is", err)
			return err
		} else {
			backend = c.(*net.TCPConn)
		}

		return nil
	}
	for i := 0; i < RetryMax && backend == nil; i++ {
		if r := connectBackend(); err == nil {
			err = r
		}
	}
	if backend == nil {
		ol.W(ctx, "proxy failed for no backend, err is", err)
		return
	}
	defer backend.Close()
	ol.T(ctx, fmt.Sprintf("proxy %v to %v, useProxyProtocol=%v",
		client.RemoteAddr(), backend.RemoteAddr(), v.conf.Rtmp.UseRtmpProxy))

	// proxy c to conn
	var wg sync.WaitGroup

	var nr, nw int64
	defer func() {
		ol.T(ctx, fmt.Sprintf("proxy client ok, read=%v, write=%v", nr, nw))
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer client.Close()
		if nw, err = io.Copy(client, backend); err != nil {
			ol.E(ctx, fmt.Sprintf("proxy rtmp<=backend failed, nn=%v, err is %v", nw, err))
			return
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		defer client.Close()

		// write proxy header.
		// @see https://github.com/ossrs/go-oryx/wiki/RtmpProxy
		if v.conf.Rtmp.UseRtmpProxy {
			var ip []byte
			if addr, ok := client.RemoteAddr().(*net.TCPAddr); ok {
				// TODO: support ipv6 client.
				ip = addr.IP.To4()
			}

			b := &bytes.Buffer{}
			b.WriteByte(0xF3)
			binary.Write(b, binary.BigEndian, uint16(len(ip)))
			b.Write(ip)
			//ol.T(ctx, "write rtmp protocol", b.Bytes())

			if _, err = backend.Write(b.Bytes()); err != nil {
				ol.E(ctx, fmt.Sprintf("write proxy failed, b=%v, err is %v", b.Bytes(), err))
				return
			}
		}

		if nr, err = io.Copy(backend, client); err != nil {
			ol.E(ctx, fmt.Sprintf("proxy rtmp=>backend failed, nn=%v, err is %v", nr, err))
			return
		}
	}()

	go func() {
		<-ctx.Done()
		client.Close()
	}()

	wg.Wait()
	return
}

const (
	Success oh.SystemError = 0
	// error when api proxy parse parameters.
	ApiProxyQuery oh.SystemError = 100 + iota
)

func main() {
	var err error

	// for shell.
	var backend, port string
	flag.StringVar(&backend, "b", "", "The backend server tcp://host:port, optional.")
	flag.StringVar(&port, "l", "", "The listen tcp://host:port, optional.")

	confFile := oo.ParseArgv("../conf/rtmplb.json", Version(), signature)
	fmt.Println("RTMPLB is the load-balance for RTMP streaming, config is", confFile)

	conf := &RtmpLbConfig{}
	if err = conf.Loads(confFile); err != nil {
		ol.E(nil, "Loads config failed, err is", err)
		return
	}
	defer conf.Close()

	// override by shell.
	if port != "" {
		conf.Rtmp.Listen = port
	}
	if backend != "" {
		conf.Rtmp.Backend = append(conf.Rtmp.Backend, backend)
	}

	ctx, cancel := context.WithCancel(context.Background())
	ol.T(ctx, fmt.Sprintf("Config ok, %v", conf))

	var listener *TcpListeners
	if listener, err = NewTcpListeners([]string{conf.Rtmp.Listen}); err != nil {
		ol.E(ctx, "create listener failed, err is", err)
		return
	}
	defer listener.Close()

	if err = listener.ListenTCP(ctx); err != nil {
		ol.E(ctx, "listen tcp failed, err is", err)
		return
	}

	proxy := NewProxy(conf)
	oh.Server = signature

	go func() {
		c := make(chan os.Signal, 1)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)

		<-c
		cancel()
	}()

	defer ol.T(ctx, "serve ok")

	// rtmp connections
	go func() {
		<-ctx.Done()
		listener.Close()
	}()

	ol.T(ctx, "rtmp accepter ready")
	defer ol.T(ctx, "rtmp accepter ok")

	for {
		var c *net.TCPConn
		if c, err = listener.AcceptTCP(); err != nil {
			if err != io.EOF {
				ol.E(ctx, "accept failed, err is", err)
			}
			break
		}

		//ol.T(ctx, "got rtmp client", c.RemoteAddr())
		go proxy.serveRtmp(ctx, c)
	}

	return
}
