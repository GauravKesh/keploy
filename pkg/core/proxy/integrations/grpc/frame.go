//go:build linux

package grpc

import (
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

// transferFrame reads one frame from rhs and writes it to lhs.
func transferFrame(ctx context.Context, _ *zap.Logger, lhs net.Conn, rhs net.Conn, sic *StreamInfoCollection, reqFromClient bool, decoder *hpack.Decoder, mocks chan<- *models.Mock) error {
	respFromServer := !reqFromClient
	framer := http2.NewFramer(lhs, rhs)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			frame, err := framer.ReadFrame()
			if err != nil {
				if err == io.EOF {
					return err
				}
				return fmt.Errorf("error reading frame %v", err)
			}

			switch frame := frame.(type) {
			case *http2.SettingsFrame:
				settingsFrame := frame
				if settingsFrame.IsAck() {
					// Transfer Ack.
					if err := framer.WriteSettingsAck(); err != nil {
						return fmt.Errorf("could not write ack for settings frame: %v", err)
					}
				} else {
					var settingsCollection []http2.Setting
					err = settingsFrame.ForeachSetting(func(setting http2.Setting) error {
						settingsCollection = append(settingsCollection, setting)
						return nil
					})
					if err != nil {
						return fmt.Errorf("could not read settings from settings frame: %v", err)
					}

					if err := framer.WriteSettings(settingsCollection...); err != nil {
						return fmt.Errorf("could not write settings fraame: %v", err)
					}
				}
			case *http2.HeadersFrame:
				headersFrame := frame
				streamID := headersFrame.StreamID
				err := framer.WriteHeaders(http2.HeadersFrameParam{
					StreamID:      streamID,
					BlockFragment: headersFrame.HeaderBlockFragment(),
					EndStream:     headersFrame.StreamEnded(),
					EndHeaders:    headersFrame.HeadersEnded(),
					PadLength:     0,
					Priority:      headersFrame.Priority,
				})
				if err != nil {
					return fmt.Errorf("could not write headers frame: %v", err)
				}
				pseudoHeaders, ordinaryHeaders, err := extractHeaders(headersFrame, decoder)
				if err != nil {
					return fmt.Errorf("could not extract headers from frame: %v", err)
				}

				if reqFromClient {
					sic.AddHeadersForRequest(streamID, pseudoHeaders, true)
					sic.AddHeadersForRequest(streamID, ordinaryHeaders, false)

				} else if respFromServer {
					if headersFrame.StreamEnded() {
						// Trailers — filter grpc-* as trailer, rest as normal headers
						pseudoNormal, pseudoTrailer := splitGrpcTrailerHeaders(pseudoHeaders)
						ordinaryNormal, ordinaryTrailer := splitGrpcTrailerHeaders(ordinaryHeaders)

						// Add "normal" parts as headers (still appears in trailers, but your system might need this distinction)
						sic.AddHeadersForResponse(streamID, pseudoNormal, true, false)
						sic.AddHeadersForResponse(streamID, ordinaryNormal, false, false)

						// Add "grpc-" keys as actual trailers
						sic.AddHeadersForResponse(streamID, pseudoTrailer, true, true)
						sic.AddHeadersForResponse(streamID, ordinaryTrailer, false, true)

					} else {
						// Just regular headers
						sic.AddHeadersForResponse(streamID, pseudoHeaders, true, false)
						sic.AddHeadersForResponse(streamID, ordinaryHeaders, false, false)
					}
				}
				// The trailers frame has been received. The stream has been closed by the server.
				// Capture the mock and clear the map, as the stream ID can be reused by client.
				if respFromServer && headersFrame.StreamEnded() {
					sic.PersistMockForStream(ctx, streamID, mocks)
					sic.ResetStream(streamID)
				}

			case *http2.DataFrame:
				dataFrame := frame
				err := framer.WriteData(dataFrame.StreamID, dataFrame.StreamEnded(), dataFrame.Data())
				if err != nil {
					return fmt.Errorf("could not write data frame: %v", err)
				}
				if reqFromClient {
					// Capturing the request timestamp
					sic.ReqTimestampMock = time.Now()

					sic.AddPayloadForRequest(dataFrame.StreamID, dataFrame.Data())
				} else if respFromServer {
					// Capturing the response timestamp
					sic.ResTimestampMock = time.Now()

					sic.AddPayloadForResponse(dataFrame.StreamID, dataFrame.Data())
				}
			case *http2.PingFrame:
				pingFrame := frame
				err := framer.WritePing(pingFrame.IsAck(), pingFrame.Data)
				if err != nil {
					return fmt.Errorf("could not write ACK for ping: %v", err)
				}
			case *http2.WindowUpdateFrame:
				windowUpdateFrame := frame
				err := framer.WriteWindowUpdate(windowUpdateFrame.StreamID, windowUpdateFrame.Increment)
				if err != nil {
					return fmt.Errorf("could not write window tools frame: %v", err)
				}
			case *http2.ContinuationFrame:
				continuationFrame := frame
				err := framer.WriteContinuation(continuationFrame.StreamID, continuationFrame.HeadersEnded(),
					continuationFrame.HeaderBlockFragment())
				if err != nil {
					return fmt.Errorf("could not write continuation frame: %v", err)
				}
			case *http2.PriorityFrame:
				priorityFrame := frame
				err := framer.WritePriority(priorityFrame.StreamID, priorityFrame.PriorityParam)
				if err != nil {
					return fmt.Errorf("could not write priority frame: %v", err)
				}
			case *http2.RSTStreamFrame:
				rstStreamFrame := frame
				err := framer.WriteRSTStream(rstStreamFrame.StreamID, rstStreamFrame.ErrCode)
				if err != nil {
					return fmt.Errorf("could not write reset stream frame: %v", err)
				}
			case *http2.GoAwayFrame:
				goAwayFrame := frame
				err := framer.WriteGoAway(goAwayFrame.StreamID, goAwayFrame.ErrCode, goAwayFrame.DebugData())
				if err != nil {
					return fmt.Errorf("could not write GoAway frame: %v", err)
				}
			case *http2.PushPromiseFrame:
				pushPromiseFrame := frame
				err := framer.WritePushPromise(http2.PushPromiseParam{
					StreamID:      pushPromiseFrame.StreamID,
					PromiseID:     pushPromiseFrame.PromiseID,
					BlockFragment: pushPromiseFrame.HeaderBlockFragment(),
					EndHeaders:    pushPromiseFrame.HeadersEnded(),
					PadLength:     0,
				})
				if err != nil {
					return fmt.Errorf("could not write PushPromise frame: %v", err)
				}
			}
		}
	}
}

func splitGrpcTrailerHeaders(headers map[string]string) (normal map[string]string, trailer map[string]string) {
	normal = make(map[string]string)
	trailer = make(map[string]string)
	for k, v := range headers {
		if strings.HasPrefix(k, "grpc-") {
			trailer[k] = v
		} else {
			normal[k] = v
		}
	}
	return
}

// constants for dynamic table size
const (
	KmaxDynamicTableSize = 4096
)

func extractHeaders(frame *http2.HeadersFrame, decoder *hpack.Decoder) (pseudoHeaders, ordinaryHeaders map[string]string, err error) {
	hf, err := decoder.DecodeFull(frame.HeaderBlockFragment())
	if err != nil {
		return nil, nil, fmt.Errorf("could not decode headers: %v", err)
	}

	pseudoHeaders = make(map[string]string)
	ordinaryHeaders = make(map[string]string)

	for _, header := range hf {
		if header.IsPseudo() {
			pseudoHeaders[header.Name] = header.Value
		} else {
			ordinaryHeaders[header.Name] = header.Value
		}
	}

	return pseudoHeaders, ordinaryHeaders, nil
}
