package ios

import (
	"bytes"
	"fmt"
	log "github.com/sirupsen/logrus"
	"golang.org/x/net/http2"
	"io"
	"sync/atomic"
)

type StreamId uint32

const (
	InitStream   = StreamId(0)
	ClientServer = StreamId(1)
	ServerClient = StreamId(3)
)

type HttpConnection struct {
	framer             *http2.Framer
	clientServerStream *bytes.Buffer
	serverClientStream *bytes.Buffer
	closer             io.Closer
	csIsOpen           *atomic.Bool
	scIsOpen           *atomic.Bool
}

func (r *HttpConnection) Close() error {
	return r.closer.Close()
}

func NewHttpConnection(rw io.ReadWriteCloser) (*HttpConnection, error) {
	framer := http2.NewFramer(rw, rw)

	_, err := rw.Write([]byte("PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"))
	if err != nil {
		return nil, err
	}

	err = framer.WriteSettings(
		http2.Setting{ID: http2.SettingMaxConcurrentStreams, Val: 100},
		http2.Setting{ID: http2.SettingInitialWindowSize, Val: 1048576},
	)
	if err != nil {
		return nil, err
	}

	err = framer.WriteWindowUpdate(uint32(InitStream), 983041)
	if err != nil {
		return nil, err
	}
	//
	frame, err := framer.ReadFrame()
	if err != nil {
		return nil, err
	}
	if frame.Header().Type == http2.FrameSettings {
		settings := frame.(*http2.SettingsFrame)
		v, ok := settings.Value(http2.SettingInitialWindowSize)
		if ok {
			framer.SetMaxReadFrameSize(v)
		}
		err := framer.WriteSettingsAck()
		if err != nil {
			return nil, err
		}
	} else {
		log.WithField("frame", frame.Header().String()).
			Warn("expected setttings frame")
	}

	return &HttpConnection{
		framer:             framer,
		clientServerStream: bytes.NewBuffer(nil),
		serverClientStream: bytes.NewBuffer(nil),
		closer:             rw,
		csIsOpen:           &atomic.Bool{},
		scIsOpen:           &atomic.Bool{},
	}, nil
}

func (r *HttpConnection) ReadClientServerStream(p []byte) (int, error) {
	for r.clientServerStream.Len() < len(p) {
		err := r.readDataFrame()
		if err != nil {
			return 0, err
		}
	}
	return r.clientServerStream.Read(p)
}

func (r *HttpConnection) WriteClientServerStream(p []byte) (int, error) {
	return r.write(p, uint32(ClientServer), r.csIsOpen)
}

func (r *HttpConnection) WriteServerClientStream(p []byte) (int, error) {
	return r.write(p, uint32(ServerClient), r.scIsOpen)
}

func (r *HttpConnection) write(p []byte, stream uint32, isOpen *atomic.Bool) (int, error) {
	if isOpen.CompareAndSwap(false, true) {
		err := r.framer.WriteHeaders(http2.HeadersFrameParam{
			StreamID:   stream,
			EndHeaders: true,
		})
		if err != nil {
			return 0, fmt.Errorf("could not send headers. %w", err)
		}
	}
	return r.Write(p, stream)
}

func (r *HttpConnection) Write(p []byte, streamId uint32) (int, error) {
	err := r.framer.WriteData(streamId, false, p)
	if err != nil {
		return 0, err
	}
	return len(p), nil
}

func (r *HttpConnection) readDataFrame() error {
	for {
		f, err := r.framer.ReadFrame()
		if err != nil {
			return err
		}
		log.WithField("frame", f.Header().String()).Debug("received frame")
		switch f.Header().Type {
		case http2.FrameData:
			d := f.(*http2.DataFrame)
			switch d.StreamID {
			case 1:
				r.clientServerStream.Write(d.Data())
			case 3:
				r.serverClientStream.Write(d.Data())
			default:
				panic(fmt.Errorf("unknown stream id %d", d.StreamID))
			}
			return nil
		case http2.FrameGoAway:
			return fmt.Errorf("received GOAWAY")
		case http2.FrameSettings:
			s := f.(*http2.SettingsFrame)
			if s.Flags&http2.FlagSettingsAck != http2.FlagSettingsAck {
				err := r.framer.WriteSettingsAck()
				if err != nil {
					return err
				}
			}
		case http2.FrameWindowUpdate:
			w := f.(*http2.WindowUpdateFrame)
			log.Printf("Window increment %d", w.Increment)
		default:
			break
		}
	}
}

func (r *HttpConnection) ReadServerClientStream(p []byte) (int, error) {
	for r.serverClientStream.Len() < len(p) {
		err := r.readDataFrame()
		if err != nil {
			return 0, err
		}
	}
	return r.serverClientStream.Read(p)
}

type HttpStreamReadWriter struct {
	h        *HttpConnection
	streamId uint32
}

func NewStreamReadWriter(h *HttpConnection, streamId StreamId) HttpStreamReadWriter {
	return HttpStreamReadWriter{
		h:        h,
		streamId: uint32(streamId),
	}
}

func (h HttpStreamReadWriter) Read(p []byte) (n int, err error) {
	if h.streamId == 1 {
		return h.h.ReadClientServerStream(p)
	} else if h.streamId == 3 {
		return h.h.ReadServerClientStream(p)
	}
	panic(nil)
}

func (h HttpStreamReadWriter) Write(p []byte) (n int, err error) {
	if h.streamId == 1 {
		return h.h.WriteClientServerStream(p)
	} else if h.streamId == 3 {
		return h.h.WriteServerClientStream(p)
	}
	panic("implement me")
}
