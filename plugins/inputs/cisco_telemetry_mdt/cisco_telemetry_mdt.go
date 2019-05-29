package cisco_telemetry_mdt

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"

	"google.golang.org/grpc/metadata"

	"github.com/golang/protobuf/proto"
	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/internal"
	internaltls "github.com/influxdata/telegraf/internal/tls"
	"github.com/influxdata/telegraf/plugins/inputs"
	"github.com/influxdata/telegraf/plugins/inputs/cisco_telemetry_mdt/ems"
	dialout "github.com/influxdata/telegraf/plugins/inputs/cisco_telemetry_mdt/mdt_dialout"
	"github.com/influxdata/telegraf/plugins/inputs/cisco_telemetry_mdt/telemetry"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const (
	// Maximum telemetry payload size (in bytes) to accept for GRPC dialout transport
	tcpMaxMsgLen uint32 = 1024 * 1024

	// IOS XR EMS dialin telemetry GPBKV encoding
	grpcEncodeGPBKV int64 = 3
)

// CiscoTelemetryMDT plugin for IOS XR, IOS XE and NXOS platforms
type CiscoTelemetryMDT struct {
	// Common configuration
	Transport      string
	ServiceAddress string `toml:"service_address"`

	// GRPC dialin settings
	Username     string
	Password     string
	Subscription string
	Redial       internal.Duration
	MaxMsgSize   int `toml:"max_msg_size"`

	// GRPC TLS settings
	EnableTLS          bool     `toml:"enable_tls"`
	TLSCA              string   `toml:"tls_ca"`
	TLSCert            string   `toml:"tls_cert"`
	TLSKey             string   `toml:"tls_key"`
	InsecureSkipVerify bool     `toml:"insecure_skip_verify"`
	TLSAllowedCACerts  []string `toml:"tls_allowed_cacerts"`

	// Internal listener / client handle
	grpcServer *grpc.Server
	listener   net.Listener

	// Internal state
	acc    telegraf.Accumulator
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// Start the Cisco MDT service
func (c *CiscoTelemetryMDT) Start(acc telegraf.Accumulator) error {
	var err error
	var ctx context.Context
	c.acc = acc
	ctx, c.cancel = context.WithCancel(context.Background())

	switch c.Transport {
	case "tcp-dialout":
		c.listener, err = net.Listen("tcp", c.ServiceAddress)
		if err != nil {
			return err
		}

		// TCP dialout server accept routine
		c.wg.Add(1)
		go c.acceptTCPDialoutClients(ctx)

	case "grpc-dialout":
		var opts []grpc.ServerOption

		if c.EnableTLS {
			tlsConfig, err := (&internaltls.ServerConfig{
				TLSCert:           c.TLSCert,
				TLSKey:            c.TLSKey,
				TLSAllowedCACerts: c.TLSAllowedCACerts,
			}).TLSConfig()
			if err != nil {
				return err
			}

			opts = append(opts, grpc.Creds(credentials.NewTLS(tlsConfig)))
		}

		if c.MaxMsgSize > 0 {
			opts = append(opts, grpc.MaxRecvMsgSize(c.MaxMsgSize))
		}

		c.listener, err = net.Listen("tcp", c.ServiceAddress)
		if err != nil {
			return err
		}

		c.grpcServer = grpc.NewServer(opts...)
		dialout.RegisterGRPCMdtDialoutServer(c.grpcServer, c)

		c.wg.Add(1)
		go func() {
			c.grpcServer.Serve(c.listener)
			c.wg.Done()
		}()

	case "grpc-dialin":
		var opts []grpc.DialOption
		ctx = metadata.AppendToOutgoingContext(ctx, "username", c.Username, "password", c.Password)

		if c.EnableTLS {
			tlsConfig, err := (&internaltls.ClientConfig{
				TLSCA:              c.TLSCA,
				TLSCert:            c.TLSCert,
				TLSKey:             c.TLSKey,
				InsecureSkipVerify: c.InsecureSkipVerify,
			}).TLSConfig()
			if err != nil {
				return err
			}

			opts = append(opts, grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
		} else {
			opts = append(opts, grpc.WithInsecure())
		}

		if c.MaxMsgSize > 0 {
			opts = append(opts, grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(c.MaxMsgSize)))
		}

		client, err := grpc.DialContext(ctx, c.ServiceAddress, opts...)
		if err != nil {
			return fmt.Errorf("failed to dial Cisco MDT: %v", err)
		}

		// Dialin client telemetry stream reading routine
		c.wg.Add(1)
		go c.subscribeMDTDialinDevice(ctx, client)

	default:
		return fmt.Errorf("invalid Cisco MDT transport: %s", c.Transport)
	}

	log.Printf("I! Started Cisco MDT service on %s", c.ServiceAddress)

	return nil
}

// AcceptTCPDialoutClients defines the TCP dialout server main routine
func (c *CiscoTelemetryMDT) acceptTCPDialoutClients(ctx context.Context) {
	// Keep track of all active connections, so we can close them if necessary
	var mutex sync.Mutex
	clients := make(map[net.Conn]struct{})

	for ctx.Err() == nil {
		conn, err := c.listener.Accept()
		if err != nil {
			if ctx.Err() == nil {
				c.acc.AddError(fmt.Errorf("failed to accept TCP connection: %v", err))
			}
			continue
		}

		mutex.Lock()
		clients[conn] = struct{}{}
		mutex.Unlock()

		// Individual client connection routine
		c.wg.Add(1)
		go func() {
			log.Printf("D! Accepted Cisco MDT TCP dialout connection from %s", conn.RemoteAddr())

			// TCP Dialout telemetry framing header
			var hdr struct {
				MsgType       uint16
				MsgEncap      uint16
				MsgHdrVersion uint16
				MsgFlags      uint16
				MsgLen        uint32
			}

			var payload bytes.Buffer

			for ctx.Err() == nil {
				// Read and validate dialout telemetry header
				if err := binary.Read(conn, binary.BigEndian, &hdr); err != nil {
					if ctx.Err() == nil && err != io.EOF {
						c.acc.AddError(fmt.Errorf("unable to read dialout header: %v", err))
					}
					break
				}

				maxMsgSize := tcpMaxMsgLen
				if c.MaxMsgSize > 0 {
					maxMsgSize = uint32(c.MaxMsgSize)
				}

				if hdr.MsgLen > maxMsgSize {
					c.acc.AddError(fmt.Errorf("dialout packet too long: %v", hdr.MsgLen))
					break
				}

				if hdr.MsgFlags != 0 {
					c.acc.AddError(fmt.Errorf("Invalid dialout flags: %v", hdr.MsgFlags))
					break
				}

				// Read and handle telemetry packet
				payload.Reset()
				if size, err := payload.ReadFrom(io.LimitReader(conn, int64(hdr.MsgLen))); size != int64(hdr.MsgLen) {
					if ctx.Err() == nil {
						if err != nil {
							c.acc.AddError(fmt.Errorf("TCP dialout I/O error: %v", err))
						} else {
							c.acc.AddError(fmt.Errorf("TCP dialout premature EOF"))
						}
					}
					break
				}

				c.handleTelemetry(payload.Bytes())
			}

			log.Printf("D! Closed Cisco MDT TCP dialout connection from %s", conn.RemoteAddr())

			mutex.Lock()
			delete(clients, conn)
			mutex.Unlock()

			conn.Close()
			c.wg.Done()
		}()
	}

	// Close all remaining client connections
	mutex.Lock()
	for client := range clients {
		if err := client.Close(); err != nil {
			log.Printf("E! Failed to close TCP dialout client: %v", err)
		}
	}
	mutex.Unlock()

	c.listener.Close()
	c.wg.Done()
}

// MdtDialout RPC server method for grpc-dialout transport
func (c *CiscoTelemetryMDT) MdtDialout(stream dialout.GRPCMdtDialout_MdtDialoutServer) error {
	peer, peerOK := peer.FromContext(stream.Context())
	if peerOK {
		log.Printf("D! Accepted Cisco MDT GRPC dialout connection from %s", peer.Addr)
	}

	for {
		packet, err := stream.Recv()
		if err != nil {
			if err != io.EOF {
				c.acc.AddError(fmt.Errorf("GRPC dialout receive error: %v", err))
			}
			break
		}

		if len(packet.Data) == 0 && len(packet.Errors) != 0 {
			c.acc.AddError(fmt.Errorf("GRPC dialout error: %s", packet.Errors))
			break
		}

		c.handleTelemetry(packet.Data)
	}

	if peerOK {
		log.Printf("D! Closed Cisco MDT GRPC dialout connection from %s", peer.Addr)
	}

	return nil
}

// SubscribeMDTDialinDevice and extract GPB telemetry data
func (c *CiscoTelemetryMDT) subscribeMDTDialinDevice(ctx context.Context, client *grpc.ClientConn) {
	for ctx.Err() == nil {
		request := &ems.CreateSubsArgs{
			ReqId:    1,
			Encode:   grpcEncodeGPBKV,
			Subidstr: c.Subscription,
		}
		client := ems.NewGRPCConfigOperClient(client)
		stream, err := client.CreateSubs(ctx, request)
		if err != nil {
			c.acc.AddError(fmt.Errorf("GRPC dialin subscription failed: %v", err))
		} else {
			log.Printf("D! Subscribed to Cisco MDT device %s", c.ServiceAddress)

			// After subscription is setup, read and handle telemetry packets
			for ctx.Err() == nil {
				packet, err := stream.Recv()
				if err != nil {
					break
				}

				if len(packet.Errors) != 0 {
					c.acc.AddError(fmt.Errorf("GRPC dialin error: %s", packet.Errors))
				} else {
					c.handleTelemetry(packet.Data)
				}
			}

			if err != nil && err != io.EOF {
				c.acc.AddError(fmt.Errorf("GRPC dialin subscription receive error: %v", err))
			}

			log.Printf("D! Connection to Cisco MDT device %s closed", c.ServiceAddress)
		}

		if c.Redial.Duration.Nanoseconds() <= 0 {
			break
		}

		select {
		case <-ctx.Done():
		case <-time.After(c.Redial.Duration):
		}
	}

	client.Close()
	c.wg.Done()
}

// Handle telemetry packet from any transport, decode and add as measurement
func (c *CiscoTelemetryMDT) handleTelemetry(data []byte) {
	var namebuf bytes.Buffer
	telemetry := &telemetry.Telemetry{}
	err := proto.Unmarshal(data, telemetry)
	if err != nil {
		c.acc.AddError(fmt.Errorf("Cisco MDT failed to decode: %v", err))
		return
	}

	for _, gpbkv := range telemetry.DataGpbkv {
		var fields map[string]interface{}

		// Produce metadata tags
		var tags map[string]string

		// Top-level field may have measurement timestamp, if not use message timestamp
		measured := gpbkv.Timestamp
		if measured == 0 {
			measured = telemetry.MsgTimestamp
		}

		timestamp := time.Unix(int64(measured/1000), int64(measured%1000)*1000000)

		// Populate tags and fields from toplevel GPBKV fields "keys" and "content"
		for _, field := range gpbkv.Fields {
			switch field.Name {
			case "keys":
				tags = make(map[string]string, len(field.Fields)+2)
				tags["Producer"] = telemetry.GetNodeIdStr()
				tags["Target"] = telemetry.GetSubscriptionIdStr()
				for _, subfield := range field.Fields {
					c.parseGPBKVField(subfield, &namebuf, telemetry.EncodingPath, timestamp, tags, nil)
				}
			case "content":
				fields = make(map[string]interface{}, len(field.Fields))
				for _, subfield := range field.Fields {
					c.parseGPBKVField(subfield, &namebuf, telemetry.EncodingPath, timestamp, tags, fields)
				}
			default:
				log.Printf("I! Unexpected top-level MDT field: %s", field.Name)
			}
		}

		// Emit measurement
		if len(fields) > 0 && len(tags) > 0 && len(telemetry.EncodingPath) > 0 {
			c.acc.AddFields(telemetry.EncodingPath, fields, tags, timestamp)
		} else {
			c.acc.AddError(fmt.Errorf("Cisco MDT invalid field: encoding path or measurement empty"))
		}
	}

}

// Recursively parse GPBKV field structure into fields or tags
func (c *CiscoTelemetryMDT) parseGPBKVField(field *telemetry.TelemetryField, namebuf *bytes.Buffer,
	path string, timestamp time.Time, tags map[string]string, fields map[string]interface{}) {

	namelen := namebuf.Len()
	if namelen > 0 {
		namebuf.WriteRune('/')
	}
	namebuf.WriteString(field.Name)

	// Decode Telemetry field value if set
	var value interface{}
	switch val := field.ValueByType.(type) {
	case *telemetry.TelemetryField_BytesValue:
		value = val.BytesValue
	case *telemetry.TelemetryField_StringValue:
		value = val.StringValue
	case *telemetry.TelemetryField_BoolValue:
		value = val.BoolValue
	case *telemetry.TelemetryField_Uint32Value:
		value = val.Uint32Value
	case *telemetry.TelemetryField_Uint64Value:
		value = val.Uint64Value
	case *telemetry.TelemetryField_Sint32Value:
		value = val.Sint32Value
	case *telemetry.TelemetryField_Sint64Value:
		value = val.Sint64Value
	case *telemetry.TelemetryField_DoubleValue:
		value = val.DoubleValue
	case *telemetry.TelemetryField_FloatValue:
		value = val.FloatValue
	}

	if value != nil {
		// Distinguish between tags (keys) and fields (data) to write to
		if fields != nil {
			fields[namebuf.String()] = value
		} else {
			tags[namebuf.String()] = fmt.Sprint(value)
		}
	}

	for _, subfield := range field.Fields {
		c.parseGPBKVField(subfield, namebuf, path, timestamp, tags, fields)
	}

	namebuf.Truncate(namelen)
}

// Stop listener and cleanup
func (c *CiscoTelemetryMDT) Stop() {
	c.cancel()
	if c.grpcServer != nil {
		// Stop server and terminate all running dialout routines
		c.grpcServer.Stop()
	}
	if c.listener != nil {
		c.listener.Close()
	}
	c.wg.Wait()

	log.Println("I! Stopped Cisco MDT service on ", c.ServiceAddress)
}

const sampleConfig = `
  ## Telemetry transport (one of: tcp-dialout, grpc-dialout, grpc-dialin)
  transport = "grpc-dialout"

  ## Address and port to host telemetry listener on (dialout) or address to connect to (dialin)
  service_address = ":57000"

  ## Enable TLS for transport
  # enable_tls = true

  ## grpc-dialin: define credentials and subscription
  # username = "cisco"
  # password = "cisco"
  # subscription = "subscription"
  # redial = "10s"

  ## grpc-dialin: define TLS CA to authenticate the device
  # tls_ca = "/etc/telegraf/ca.pem"
  # insecure_skip_verify = true

  ## grpc-dialin: define client-side TLS certificate & key to authenticate to the device
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"


  ## grpc-dialout: define TLS certificate and key
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"

  ## grpc-dialout: enable TLS client authentication and define allowed CA certificates
  # tls_allowed_cacerts = ["/etc/telegraf/clientca.pem"]
`

// SampleConfig of plugin
func (c *CiscoTelemetryMDT) SampleConfig() string {
	return sampleConfig
}

// Description of plugin
func (c *CiscoTelemetryMDT) Description() string {
	return "Cisco model-driven telemetry (MDT) input plugin for IOS XR, IOS XE and NX-OS platforms"
}

// Gather plugin measurements (unused)
func (c *CiscoTelemetryMDT) Gather(_ telegraf.Accumulator) error {
	return nil
}

func init() {
	inputs.Add("cisco_telemetry_mdt", func() telegraf.Input {
		return &CiscoTelemetryMDT{
			Transport:      "grpc-dialout",
			ServiceAddress: ":57000",
			Redial:         internal.Duration{Duration: 10 * time.Second},
		}
	})
}
