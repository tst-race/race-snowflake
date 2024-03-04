//
// Copyright 2023 Two Six Technologies
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//

// CommsPluginTwoSix Interface. Is a Golang  implementation of the RACE T2 Plugin. Will
// perform obfuscated communication for the RACE system.

package main

import "C"

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/xtaci/smux"

	commsshims "shims"

	sfb "github.com/RACECAR-GU/snowflake/broker"
	sfc "github.com/RACECAR-GU/snowflake/client/lib"
	sfp "github.com/RACECAR-GU/snowflake/proxy"
	sfs "github.com/RACECAR-GU/snowflake/server/lib"
)

var ACK = []byte("ACKNOWLEDGED")

// A CommsConn represents a logical connection connecting two RACE nodes
type CommsConn interface {
	// Returns the link ID of the connection
	GetLinkId() string
	// Returns the link type of the connection
	GetLinkType() commsshims.LinkType
	// Adds a connection ID to the connection, returning the new number of IDs
	AddConnectionId(connectionId string) int
	// Removes a connection ID from the connection, returning the new number of IDs
	RemoveConnectionId(connectionId string) int
	// Gets the list of connection IDs associated with the connection
	GetConnectionIds() []string
	// Closes the connection
	Close() error
	// Writes the given raw message payload to the connection
	Write(msg []byte) error
	// Starts receiving messages over the connection. This method should block
	// until the connection has been closed. It will be invoked in a goroutine.
	Receive(plugin *overwrittenMethodsOnCommsPluginTwoSix)
}

// Attributes common to unicast and multicast connection types
type commsConnCommon struct {
	connectionIdsAsMap map[string]bool
	connectionIdsMutex sync.RWMutex
	LinkId             string
	LinkType           commsshims.LinkType
}

// Returns the link ID of the given connection
func (conn *commsConnCommon) GetLinkId() string {
	return conn.LinkId
}

// Returns the link type of the given connection
func (conn *commsConnCommon) GetLinkType() commsshims.LinkType {
	return conn.LinkType
}

// Adds a connection ID to the connection, returning the new number of IDs
func (conn *commsConnCommon) AddConnectionId(connectionId string) int {
	conn.connectionIdsMutex.Lock()
	defer conn.connectionIdsMutex.Unlock()
	if connectionId != "" {
		conn.connectionIdsAsMap[connectionId] = true
	} else {
		logWarning("commsConnCommon::AddConnectionId: invalid connection ID is empty string.")
	}
	return len(conn.connectionIdsAsMap)
}

// Removes a connection ID from the connection, returning the new number of IDs
func (conn *commsConnCommon) RemoveConnectionId(connectionId string) int {
	conn.connectionIdsMutex.Lock()
	defer conn.connectionIdsMutex.Unlock()
	delete(conn.connectionIdsAsMap, connectionId)
	return len(conn.connectionIdsAsMap)
}

// Gets the list of connection IDs associated with the connection
func (conn *commsConnCommon) GetConnectionIds() []string {
	conn.connectionIdsMutex.RLock()
	defer conn.connectionIdsMutex.RUnlock()
	var keys []string
	for key := range conn.connectionIdsAsMap {
		keys = append(keys, key)
	}
	return keys
}

// Unicast/direct connection type
type commsConnUnicast struct {
	commsConnCommon
	Host          string
	Port          int
	ClientFactory *sfc.Transport
	Ice           string
	SavedConn     net.Conn
	SavedConnID   string
}

// Unicast/direct connection parameters
type unicastProfile struct {
	Hostname string `json:"hostname"`
	Port     int    `json:"port"`
	Ice      string `json:"ice"`
}

// Creates a new unicast connection instance
func newUnicastConn(newConnectionId string, linkType commsshims.LinkType, linkId string, linkProfile string) (CommsConn, error) {
	var profile unicastProfile
	err := json.Unmarshal([]byte(linkProfile), &profile)
	if err != nil {
		logError("failed to parse link profile json: ", err.Error())
		return nil, err
	}
	if newConnectionId == "" {
		logError("newUnicastConn: invalid connection ID is empty string")
		return nil, err
	}

	// Create the connection object
	connection := commsConnUnicast{
		commsConnCommon: commsConnCommon{
			connectionIdsAsMap: map[string]bool{newConnectionId: true},
			LinkId:             linkId,
			LinkType:           linkType,
		},
		Host: profile.Hostname,
		Port: profile.Port,
		Ice:  profile.Ice,
	}

	logInfo("Using ICE servers: ", profile.Ice)

	if linkType == commsshims.LT_SEND || linkType == commsshims.LT_BIDI {
		transport, err := sfc.NewSnowflakeClient(fmt.Sprintf("http://%v:%v", connection.Host, connection.Port), "", []string{profile.Ice}, true, 1)
		if err != nil {
			logError("failed to create Snowflake transport: ", err.Error())
		}
		connection.ClientFactory = transport
	}

	if linkType == commsshims.LT_RECV || linkType == commsshims.LT_BIDI {
	}

	logDebug("OpenConnection:opened connection on host \"", connection.Host, "\" and port \"", connection.Port, "\"")
	return &connection, nil
}

// Close the socket associated with the given Connection
// (This will cause the active goroutine that
// is listening on this socket to end.)
func (connection *commsConnUnicast) Close() error {
	return nil
}

// Open a connection to the destination host and write the given payload message
func (connection *commsConnUnicast) Write(msg []byte) error {
	logDebug("Sending Message to ", connection.Host, ":", connection.Port)

	var conn net.Conn
	var err error
	reused := false
	if connection.SavedConn != nil && connection.SavedConnID == connection.GetLinkId() {
		logDebug("connectionMonitor: Reusing connection. Saved host: ", connection.Host, " port: ", connection.Port)
		conn = connection.SavedConn
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		reused = true
	} else {
		logDebug("connectionMonitor: Dialing up a new connection, host: ", connection.Host, " port: ", connection.Port)
		// connect to this Socket
		conn, err = connection.ClientFactory.Dial()
		if err != nil {
			logError("Error Connecting to Send Socket: ", err.Error())
			return err
		}
		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		connection.SavedConn = conn
		connection.SavedConnID = connection.GetLinkId()
	}

	// Send Message to Socket
	n, err := conn.Write(msg)
	if err != nil {
		logError("Error Writing Message to Send Socket: ", err.Error())
		if reused {
			connection.SavedConn.Close()
			connection.SavedConn = nil
			logDebug("Connection was reused - trying a fresh connection")
			return connection.Write(msg)
		}
		return err
	}

	ack := make([]byte, len(ACK))
	conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	_, err = io.ReadFull(conn, ack[:])
	if err != nil {
		logError("failed to read full ack")
		if reused {
			connection.SavedConn.Close()
			connection.SavedConn = nil
			logDebug("Connection was reused - trying a fresh connection")
			return connection.Write(msg)
		}
		return fmt.Errorf("failed to read full ack")
	}
	if !bytes.Equal(ack, ACK) {
		logError("Bad message ack")
		if reused {
			connection.SavedConn.Close()
			connection.SavedConn = nil
			logDebug("Connection was reused - trying a fresh connection")
			return connection.Write(msg)
		}
		return fmt.Errorf("Bad message ack")
	}
	logDebug("connectionMonitor: Sent ", n, " bytes to ", connection.Host, ":", connection.Port, " via ", connection.GetLinkId())
	return err
}

func proxyLoop(ln net.Listener) {
	for true {
		c1, err := ln.Accept()
		if err != nil {
			logError("proxyLoop: Error accepting broker connection: ", err.Error())
			os.Exit(1)
		}
		c2, err := net.Dial("tcp", "127.0.0.1:8888")
		if err != nil {
			logError("proxyLoop: Error dialing broker: ", err.Error())
			os.Exit(1)
		}

		copyer := func(dst io.ReadWriteCloser, src io.ReadWriteCloser) {
			defer dst.Close()
			defer src.Close()
			_, err := io.Copy(dst, src)
			if err != nil {
				logError("proxyLoop: Error in copyer: ", err.Error())
			}
		}
		go copyer(c1, c2)
		go copyer(c2, c1)
	}
}

// Open a server socket and accept incoming messages. All received messages will be forwarded
// to the given plugin. This must be executed within a goroutine.
func (connection *commsConnUnicast) Receive(plugin *overwrittenMethodsOnCommsPluginTwoSix) {

	// Forward connections from connection host and port to this node's
	// snowflake broker
	brokerLn, err := net.Listen("tcp", fmt.Sprintf("%v:%v", connection.Host, connection.Port))
	logDebug("connectionMonitor: Listening on ", connection.Host, ":", connection.Port)

	if err != nil {
		logError("connectionMonitor: Error Connecting to Listen Socket: ", err.Error())
		os.Exit(1)
	}
	go proxyLoop(brokerLn)

	// Start proxy process
	proxyNum := 5
	for i := 1; i <= proxyNum; i++ {
		go func() {
			proxy := &sfp.SnowflakeProxy{
				Capacity:           100,
				StunURL:            connection.Ice,
				BrokerURL:          "http://127.0.0.1:8888",
				KeepLocalAddresses: true,
				RelayURL:           "ws://127.0.0.1:8889",
				ConnectionId:       connection.GetLinkId(),
			}
			proxy.StartProxy()
		}()
	}
	logInfo("connectionMonitor: Started ", proxyNum, " proxies.")
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) StartSnowflakeServer() {
	// Have snowflake server actually listen locally
	addr, err := net.ResolveTCPAddr("tcp", "127.0.0.1:8889")
	if err != nil {
		logError("connectionMonitor: Error resolving address: ", err.Error())
		os.Exit(1)
	}
	server := sfs.NewSnowflakeServer(nil)
	l, err := server.Listen(addr)
	if err != nil {
		logError("connectionMonitor: Error Connecting to Listen Socket: ", err.Error())
		os.Exit(1)
	}
	defer l.Close()

	for true {
		conn, err := l.Accept()
		if err != nil {
			if strings.HasSuffix(err.Error(), ": use of closed network connection") {
				// If the socket is closed, it is likely because the connection was closed from outside the goroutine, so we don't have a fit. Break the accept loop.
				logDebug("connectionMonitor: Socket closed")
				break
			}
			logError("connectionMonitor: Error accepting: ", err.Error())
			os.Exit(1)
		}

		logInfo("connectionMonitor: handling request from ", conn.RemoteAddr())
		go connHandler(conn)
	}
}

func connHandler(conn net.Conn) {
	defer conn.Close()
	for true {
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		length := make([]byte, 8)
		_, err := io.ReadFull(conn, length[:])
		if err != nil {
			if err != smux.ErrTimeout {
				logWarning("Couldn't read full message length")
			}
			return
		}

		// Read data until full packet is read or conn is closed
		data := make([]byte, binary.BigEndian.Uint64(length))
		conn.SetReadDeadline(time.Now().Add(5 * time.Minute))
		_, err = io.ReadFull(conn, data[:])
		if err != nil && err != io.EOF {
			if err != smux.ErrTimeout {
				logError("connectionMonitor: Problem reading data from socket: ", err)
			}
			return
		}
		if err == io.EOF {
			logError("Failed to read full packet. Trying anyway")
		}
		// conn.Close()

		rawData := commsshims.NewByteVector()
		for _, b := range data {
			rawData.Add(b)
		}

		// Use the link Id to get the connection Ids
		linkId := conn.RemoteAddr().String()
		logDebug("connectionMonitor: Received ", len(data), " bytes from link ", linkId)
		plugin.connectionsMutex.Lock()
		var raceConn CommsConn
		for _, connection := range plugin.connections {
			if connection.GetLinkId() == linkId && connection.GetLinkType() != commsshims.LT_SEND {
				raceConn = connection
			}
		}
		plugin.connectionsMutex.Unlock()
		if raceConn == nil {
			logError("Could not find connection Ids from link id", linkId)
			return
		}

		receivedEncPkg := commsshims.NewEncPkg(rawData)
		plugin.raceSdkReceiveEncPkgWrapper(receivedEncPkg, raceConn.GetConnectionIds())
		commsshims.DeleteByteVector(rawData)
		commsshims.DeleteEncPkg(receivedEncPkg)

		conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
		_, ackErr := conn.Write(ACK)
		if ackErr != nil {
			logDebug("ackErr: ", ackErr)
			return
		}
	}
}

// Payload for whiteboard service latest-index API
type latestIndex struct {
	Latest int `json:"latest"`
}

// Payload for whiteboard service messages API
type messages struct {
	Data   []string `json:"data"`
	Length int      `json:"length"`
}

// Forces interface to be a superset of the abstract base class
// Go type to define abstract methods.
type overwrittenMethodsOnCommsPluginTwoSix struct {
	sdk                    commsshims.IRaceSdkComms
	connections            map[string]CommsConn
	connectionsMutex       sync.RWMutex
	linkProfiles           map[string]string
	linkProperties         map[string]commsshims.LinkProperties
	channelStatuses        map[string]commsshims.ChannelStatus
	recvChannel            chan int
	nextAvailablePort      int
	hostname               string
	requestStartPortHandle uint64
	requestHostnameHandle  uint64
	ice                    string
}

// Wrapper for debug level logging using the RACE Logging API call
func logDebug(msg ...interface{}) {
	commsshims.RaceLogLogDebug("SnowflakePluginComms", fmt.Sprint(msg...), "")
}

// Wrapper for info level logging using the RACE Logging API call
func logInfo(msg ...interface{}) {
	commsshims.RaceLogLogInfo("SnowflakePluginComms", fmt.Sprint(msg...), "")
}

// Wrapper for warn level logging using the RACE Logging API call
func logWarning(msg ...interface{}) {
	commsshims.RaceLogLogWarning("SnowflakePluginComms", fmt.Sprint(msg...), "")
}

// Wrapper for error level logging using the RACE Logging API call
func logError(msg ...interface{}) {
	commsshims.RaceLogLogError("SnowflakePluginComms", fmt.Sprint(msg...), "")
}

// LinkPropSetJson represents a list of properties associated with the link. These include
// information useful for network manager/Core to choose which links to use for different types of
// communication
type LinkPropSetJson struct {
	Bandwidth_bps int     `json:"bandwidth_bps"`
	Latency_ms    int     `json:"latency_ms"`
	Loss          float32 `json:"loss"`
}

// Creates and returns a new LinkPropSet
func NewLinkPropertySet(json LinkPropSetJson) commsshims.LinkPropertySet {
	propSet := commsshims.NewLinkPropertySet()
	propSet.SetBandwidth_bps(json.Bandwidth_bps)
	propSet.SetLatency_ms(json.Latency_ms)
	propSet.SetLoss(json.Loss)
	return propSet
}

// LinkPropPairJson holds the send and receive properites of a connection. This
// includes a LinkPropSetJson for the send and receive side of the connection.
type LinkPropPairJson struct {
	Send    LinkPropSetJson `json:"send"`
	Receive LinkPropSetJson `json:"receive"`
}

// Creates and returns a new LinkPropPair
func NewLinkPropertyPair(json LinkPropPairJson) commsshims.LinkPropertyPair {
	propPair := commsshims.NewLinkPropertyPair()
	propPair.SetSend(NewLinkPropertySet(json.Send))
	propPair.SetReceive(NewLinkPropertySet(json.Receive))
	return propPair
}

// LinkPropJson represents the complete properties for a given link. This includes
// details about the link, properties (best/worst/expected cases), and what
// type of link the link is
type LinkPropJson struct {
	Linktype        string           `json:"type"`
	Reliable        bool             `json:"reliable"`
	Duration_s      int              `json:"duration_s"`
	Period_s        int              `json:"period_s"`
	Mtu             int              `json:"mtu"`
	Worst           LinkPropPairJson `json:"worst"`
	Best            LinkPropPairJson `json:"best"`
	Expected        LinkPropPairJson `json:"expected"`
	Unicast         bool             `json:"unicast"`
	Multicast       bool             `json:"multicast"`
	Supported_hints []string         `json:"supported_hints"`
}

// Unmarshal the data object into a LinkPropJson
func (t *LinkPropJson) UnmarshalJSON(data []byte) error {
	type alias LinkPropJson
	tmpSet := LinkPropSetJson{
		Bandwidth_bps: -1,
		Latency_ms:    -1,
		Loss:          -1.0,
	}
	tmpPair := LinkPropPairJson{
		Send:    tmpSet,
		Receive: tmpSet,
	}
	tmp := &alias{
		Duration_s: -1,
		Period_s:   -1,
		Mtu:        -1,
		Worst:      tmpPair,
		Best:       tmpPair,
		Expected:   tmpPair,
	}
	if err := json.Unmarshal(data, &tmp); err != nil {
		return err
	}

	*t = LinkPropJson(*tmp)
	return nil
}

// LinkProfileJson represents a LinkProfile which defines what a link is, how it
// connects, who it connects to, and which nodes an utilize the link
type LinkProfileJson struct {
	ConnectedTo []string     `json:"connectedTo"`
	UtilizedBy  []string     `json:"utilizedBy"`
	Profile     string       `json:"profile"`
	Properties  LinkPropJson `json:"properties"`
}

// Set the Sdk object and perform minimum work to
// be able to respond to incoming calls.
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) Init(pluginConfig commsshims.PluginConfig) commsshims.PluginResponse {
	logInfo("Init called")
	defer logInfo("Init returned")

	logDebug("etcDirectory: ", pluginConfig.GetEtcDirectory())
	logDebug("auxDataDirectory: ", pluginConfig.GetAuxDataDirectory())
	logDebug("loggingDirectory: ", pluginConfig.GetLoggingDirectory())
	logDebug("tmpDirectory: ", pluginConfig.GetTmpDirectory())
	logDebug("pluginDirectory: ", pluginConfig.GetPluginDirectory())

	plugin.channelStatuses = map[string]commsshims.ChannelStatus{
		DIRECT_CHANNEL_GID: commsshims.CHANNEL_UNAVAILABLE,
	}

	plugin.nextAvailablePort = 10000
	plugin.hostname = "no-hostname-provided-by-user"
	plugin.ice = "stun:stun:3478"

	plugin.connections = make(map[string]CommsConn)
	plugin.linkProfiles = make(map[string]string)
	plugin.linkProperties = make(map[string]commsshims.LinkProperties)

	bytesToWrite := commsshims.NewByteVector()
	for _, b := range []byte("Comms Golang Plugin Initialized\n") {
		bytesToWrite.Add(b)
	}
	responseStatus := plugin.sdk.WriteFile("initialized.txt", bytesToWrite).GetStatus()
	if responseStatus != commsshims.SDK_OK {
		logError("Failed to write initialized.txt")
	}
	bytesRead := plugin.sdk.ReadFile("initialized.txt")
	bytes := []byte{}
	if bytesRead.Size() >= 2<<32 {
		logError("File too large, only reading first 2^32 bytes")
	}
	for idx := 0; idx < int(bytesRead.Size()); idx++ {
		bytes = append(bytes, bytesRead.Get(idx))
	}
	stringRead := string(bytes)
	logDebug("Read Initialization File: ", stringRead)

	// Start the Snowflake server
	go plugin.StartSnowflakeServer()
	logDebug("Started Snowflake server")

	// Start the Snowflake broker
	go sfb.RunBroker(":8888")

	return commsshims.PLUGIN_OK
}

// Shutdown the plugin. Close open connections, remove state, etc.
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) Shutdown() commsshims.PluginResponse {
	logInfo("Shutdown: called")
	handle := commsshims.GetNULL_RACE_HANDLE()
	for connectionId, _ := range plugin.connections {
		plugin.CloseConnection(handle, connectionId)
	}
	logInfo("Shutdown: returned")
	return commsshims.PLUGIN_OK
}

// Get link properties for the specified link
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) GetLinkProperties(linkType commsshims.LinkType, linkId string) commsshims.LinkProperties {
	logInfo("GetLinkProperties called")
	if props, ok := plugin.linkProperties[linkId]; ok {
		return props
	}
	return commsshims.NewLinkProperties()
}

// Get connection properties for the specified connection
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) GetConnectionProperties(linkType commsshims.LinkType, connectionId string) commsshims.LinkProperties {
	logInfo("GetConnectionProperties called")
	if conn, conn_exists := plugin.connections[connectionId]; conn_exists {
		if props, link_exists := plugin.linkProperties[conn.GetLinkId()]; link_exists {
			return props
		}
	}
	return commsshims.NewLinkProperties()
}

// Send an encrypted package
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) SendPackage(handle uint64, connectionId string, encPkg commsshims.EncPkg, timeoutTimestamp float64, batchId uint64) commsshims.PluginResponse {
	defer commsshims.DeleteEncPkg(encPkg)

	logInfo("SendPackage called")
	defer logInfo("SendPackage returned")

	// get the raw bytes out of the Encrypted Package
	msg_vec := encPkg.GetRawData()
	defer commsshims.DeleteByteVector(msg_vec)
	msg := make([]byte, 8, msg_vec.Size()+8)
	msg_size := int(msg_vec.Size())
	binary.BigEndian.PutUint64(msg, uint64(msg_size))
	for i := 0; i < msg_size; i++ {
		msg = append(msg, msg_vec.Get(i))
	}

	// get the connection associated with the specified connection ID
	plugin.connectionsMutex.RLock()
	connection, ok := plugin.connections[connectionId]
	plugin.connectionsMutex.RUnlock()
	if !ok {
		logError("failed to find connection with ID = ", connectionId)
		plugin.sdk.OnPackageStatusChanged(handle, commsshims.PACKAGE_FAILED_GENERIC, commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	if err := connection.Write(msg); err != nil {
		plugin.sdk.OnPackageStatusChanged(handle, commsshims.PACKAGE_FAILED_GENERIC, commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	plugin.sdk.OnPackageStatusChanged(handle, commsshims.PACKAGE_SENT, commsshims.GetRACE_BLOCKING())
	return commsshims.PLUGIN_OK
}

// Open a connection with a given type on the specified link. Additional configuration
// info can be provided via the linkHints param.
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) OpenConnection(handle uint64, linkType commsshims.LinkType, linkId string, link_hints string, send_timeout int) commsshims.PluginResponse {
	logInfo("OpenConnection: called")
	logDebug("OpenConnection:    type = ", linkType)
	logDebug("OpenConnection:    Link ID = ", linkId)
	logDebug("OpenConnection:    link_hints = ", link_hints)
	logDebug("OpenConnection:    send_timeout = ", send_timeout)
	defer logInfo("OpenConnection: returned")

	if _, ok := plugin.linkProperties[linkId]; !ok {
		logError("OpenConnection:failed to find link with ID = ", linkId)
		return commsshims.PLUGIN_ERROR
	}

	newConnectionId := plugin.sdk.GenerateConnectionId(linkId)
	logDebug("OpenConnection: opening new connection with ID: ", newConnectionId)
	linkProperties := plugin.linkProperties[linkId]

	// Check if there is already an open connection that can be reused.
	plugin.connectionsMutex.Lock()
	for _, connection := range plugin.connections {
		if connection.GetLinkId() == linkId && connection.GetLinkType() == linkType {
			connection.AddConnectionId(newConnectionId)
			plugin.connections[newConnectionId] = connection
			plugin.connectionsMutex.Unlock()
			plugin.sdk.OnConnectionStatusChanged(handle, newConnectionId, commsshims.CONNECTION_OPEN, linkProperties, commsshims.GetRACE_BLOCKING())
			return commsshims.PLUGIN_OK
		}
	}
	plugin.connectionsMutex.Unlock()

	// Get the Link Profile with the specified ID
	linkProfile, ok := plugin.linkProfiles[linkId]
	if !ok {
		logError("OpenConnection:failed to find link profile for link with ID = ", linkId)
		plugin.sdk.OnConnectionStatusChanged(handle, newConnectionId, commsshims.CONNECTION_CLOSED, linkProperties, commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	logDebug("OpenConnection:opening connection for link profile: ", linkProfile)

	var connection CommsConn
	var err error
	if linkProperties.GetTransmissionType() == commsshims.TT_MULTICAST {
		logError("OpenConnection: wrong transmission type, failed to find link profile for link with ID = ", linkId)
		plugin.sdk.OnConnectionStatusChanged(handle, newConnectionId, commsshims.CONNECTION_CLOSED, linkProperties, commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	} else {
		connection, err = newUnicastConn(newConnectionId, linkType, linkId, linkProfile)
	}

	if err != nil {
		logError("OpenConnection: failed to create connection: ", err)
		plugin.sdk.OnConnectionStatusChanged(handle, newConnectionId, commsshims.CONNECTION_CLOSED, linkProperties, commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	// Add the connection to the Plugin's list of all active connections
	plugin.connectionsMutex.Lock()
	plugin.connections[newConnectionId] = connection
	plugin.connectionsMutex.Unlock()

	// Start a listener (in a new goroutine) if the Link Type allows receipt of messages
	if linkType == commsshims.LT_RECV || linkType == commsshims.LT_BIDI {
		logDebug("OpenConnection:Starting Connection Monitor with connection ID(s): ", strings.Join(connection.GetConnectionIds(), ", "))
		go plugin.connectionMonitor(connection)
	}

	// Update the SDK about the connection being open
	plugin.sdk.OnConnectionStatusChanged(handle, newConnectionId, commsshims.CONNECTION_OPEN, linkProperties, commsshims.GetRACE_BLOCKING())

	// Return success
	return commsshims.PLUGIN_OK

}

// Close a connection with a given ID.
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) CloseConnection(handle uint64, connectionId string) commsshims.PluginResponse {
	logInfo("CloseConnection: called")
	defer logInfo("CloseConnection: returned")

	plugin.connectionsMutex.Lock()
	defer plugin.connectionsMutex.Unlock()
	if connection, ok := plugin.connections[connectionId]; ok {
		logDebug("CloseConnection: closing connection with ID ", connectionId)
		if connection.RemoveConnectionId(connectionId) == 0 {
			logDebug("CloseConnection: last connection ID has closed, shutting down connection")
			if err := connection.Close(); err != nil {
				logError("CloseConnection: error occurred closing connection ", connectionId, ": ", err.Error())
			}
		}
		delete(plugin.connections, connectionId)

		// Update the SDK that the connection has been closed
		plugin.sdk.OnConnectionStatusChanged(handle, connectionId, commsshims.CONNECTION_CLOSED, plugin.linkProperties[connection.GetLinkId()], commsshims.GetRACE_BLOCKING())
	} else {
		logError("CloseConnection:unable to find connection with ID = ", connectionId)
		return commsshims.PLUGIN_ERROR
	}

	// Return success to the SDK
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) DestroyLink(handle uint64, linkId string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("DestroyLink: (handle: %v, link ID: %v): ", handle, linkId)
	logDebug(logPrefix, "called")
	if _, ok := plugin.linkProperties[linkId]; !ok {
		logDebug(logPrefix, "unknown link ID")
		return commsshims.PLUGIN_ERROR
	}

	plugin.sdk.OnLinkStatusChanged(handle, linkId, commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())

	// Close all the connections for the given link.
	for connectionId, connection := range plugin.connections {
		if connection.GetLinkId() == linkId {
			// Makes call to OnConnectionStatusChanged.
			plugin.CloseConnection(handle, connectionId)
		}
	}

	delete(plugin.linkProfiles, linkId)
	delete(plugin.linkProperties, linkId)

	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) CreateLink(handle uint64, channelGid string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("CreateLink: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called")

	if status, ok := plugin.channelStatuses[channelGid]; !ok || status != commsshims.CHANNEL_AVAILABLE {
		logError(logPrefix, "channel not available")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	linkId := plugin.sdk.GenerateLinkId(channelGid)
	if linkId == "" {
		logError("CreateLink: SDK failed to generate link ID. Is th channel GID valid? ", channelGid)
		return commsshims.PLUGIN_ERROR
	}

	linkProps, err := getDefaultLinkPropertiesForChannel(plugin.sdk, channelGid)
	if err != nil {
		logError(logPrefix, "failed to get default channel properties: ", err)
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	if channelGid == DIRECT_CHANNEL_GID {
		logDebug(logPrefix, "Creating TwoSix direct link with ID: ", linkId)

		linkProps.SetLinkType(commsshims.LT_RECV)

		linkProfile := unicastProfile{
			Hostname: plugin.hostname,
			Port:     plugin.nextAvailablePort,
			Ice:      plugin.ice,
		}
		plugin.nextAvailablePort += 1
		linkProfileJson, jsonErr := json.Marshal(linkProfile)
		if jsonErr != nil {
			logError(logPrefix, "failed to convert link profile to json: ", jsonErr.Error())
			plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
			return commsshims.PLUGIN_ERROR
		}
		linkProps.SetLinkAddress(string(linkProfileJson))

		plugin.linkProperties[linkId] = linkProps
		plugin.linkProfiles[linkId] = string(linkProfileJson)

		plugin.sdk.OnLinkStatusChanged(handle, linkId, commsshims.LINK_CREATED, linkProps, commsshims.GetRACE_BLOCKING())
		plugin.sdk.UpdateLinkProperties(linkId, linkProps, commsshims.GetRACE_BLOCKING())

		logDebug(logPrefix, "created direct link with link address: ", string(linkProfileJson))
	} else {
		logError(logPrefix, "invalid channel GID")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) CreateLinkFromAddress(handle uint64, channelGid string, linkAddress string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("CreateLinkFromAddress: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called")

	if status, ok := plugin.channelStatuses[channelGid]; !ok || status != commsshims.CHANNEL_AVAILABLE {
		logError(logPrefix, "channel not available")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	linkId := plugin.sdk.GenerateLinkId(channelGid)
	if linkId == "" {
		logError("CreateLinkFromAddress: SDK failed to generate link ID. Is th channel GID valid? ", channelGid)
		return commsshims.PLUGIN_ERROR
	}

	linkProps, err := getDefaultLinkPropertiesForChannel(plugin.sdk, channelGid)
	if err != nil {
		logError(logPrefix, "failed to get default channel properties: ", err)
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	linkProps.SetLinkAddress(string(linkAddress))
	if channelGid == DIRECT_CHANNEL_GID {
		logDebug(logPrefix, "Creating TwoSix direct link with ID: ", linkId)

		linkProps.SetLinkType(commsshims.LT_RECV)

		var profile unicastProfile
		err := json.Unmarshal([]byte(linkAddress), &profile)
		if err != nil {
			logError(logPrefix, "failed to parse link address: ", linkAddress, ". Error: ", err)
			plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
			return commsshims.PLUGIN_ERROR
		}

		plugin.linkProperties[linkId] = linkProps
		plugin.linkProfiles[linkId] = linkAddress

		plugin.sdk.OnLinkStatusChanged(handle, linkId, commsshims.LINK_CREATED, linkProps, commsshims.GetRACE_BLOCKING())
		plugin.sdk.UpdateLinkProperties(linkId, linkProps, commsshims.GetRACE_BLOCKING())

		logDebug(logPrefix, "Created direct link with link address: ", linkAddress)
	} else {
		logError(logPrefix, "invalid channel GID")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	logDebug("%v returned", logPrefix)
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) LoadLinkAddress(handle uint64, channelGid string, linkAddress string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("LoadLinkAddress: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called")

	if status, ok := plugin.channelStatuses[channelGid]; !ok || status != commsshims.CHANNEL_AVAILABLE {
		logError(logPrefix, "channel not available")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	linkId := plugin.sdk.GenerateLinkId(channelGid)
	if linkId == "" {
		logError("LoadLinkAddress: SDK failed to generate link ID. Is th channel GID valid? ", channelGid)
		return commsshims.PLUGIN_ERROR
	}

	linkProps, err := getDefaultLinkPropertiesForChannel(plugin.sdk, channelGid)
	if err != nil {
		logError(logPrefix, "failed to get default channel properties: ", err)
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	if channelGid == DIRECT_CHANNEL_GID {
		logDebug(logPrefix, "Loading TwoSix direct link with ID: ", linkId)

		linkProps.SetLinkType(commsshims.LT_SEND)

		var profile unicastProfile
		err := json.Unmarshal([]byte(linkAddress), &profile)
		if err != nil {
			logError(logPrefix, "failed to parse link address: ", linkAddress, ". Error: ", err)
			plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
			return commsshims.PLUGIN_ERROR
		}

		plugin.linkProperties[linkId] = linkProps
		plugin.linkProfiles[linkId] = linkAddress

		plugin.sdk.OnLinkStatusChanged(handle, linkId, commsshims.LINK_LOADED, linkProps, commsshims.GetRACE_BLOCKING())
		plugin.sdk.UpdateLinkProperties(linkId, linkProps, commsshims.GetRACE_BLOCKING())

		logDebug(logPrefix, "Loaded direct link with link address: ", linkAddress)
	} else {
		logError(logPrefix, "invalid channel GID")
		plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
		return commsshims.PLUGIN_ERROR
	}

	logDebug("%v returned", logPrefix)
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) LoadLinkAddresses(handle uint64, channelGid string, linkAddresses commsshims.StringVector) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("LoadLinkAddress: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called with link addresses: ", linkAddresses)
	logError(logPrefix, "API not supported for any TwoSix channels")
	plugin.sdk.OnLinkStatusChanged(handle, "", commsshims.LINK_DESTROYED, commsshims.NewLinkProperties(), commsshims.GetRACE_BLOCKING())
	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_ERROR
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) ActivateChannel(handle uint64, channelGid string, roleName string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("ActivateChannel: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called")

	status, ok := plugin.channelStatuses[channelGid]
	if !ok {
		logError(logPrefix, "unknown channel GID")
		return commsshims.PLUGIN_ERROR
	}

	if status == commsshims.CHANNEL_AVAILABLE {
		return commsshims.PLUGIN_OK
	}

	// TODO add request for ICE servers rather than setting default of stun:stun:3478
	if channelGid == DIRECT_CHANNEL_GID {
		plugin.channelStatuses[channelGid] = commsshims.CHANNEL_STARTING
		response := plugin.sdk.RequestCommonUserInput("hostname")
		if response.GetStatus() != commsshims.SDK_OK {
			logError("Failed to request hostname from user, direct channel cannot be used")
			plugin.channelStatuses[DIRECT_CHANNEL_GID] = commsshims.CHANNEL_FAILED
			channelProps := getDefaultChannelPropertiesForChannel(plugin.sdk, DIRECT_CHANNEL_GID)
			plugin.sdk.OnChannelStatusChanged(
				commsshims.GetNULL_RACE_HANDLE(),
				DIRECT_CHANNEL_GID,
				commsshims.CHANNEL_FAILED,
				channelProps,
				commsshims.GetRACE_BLOCKING(),
			)
			// Don't continue
			return commsshims.PLUGIN_OK
		}
		plugin.requestHostnameHandle = response.GetHandle()

		response = plugin.sdk.RequestPluginUserInput("startPort", "What is the first available port?", true)
		if response.GetStatus() != commsshims.SDK_OK {
			logWarning("Failed to request start port from user")
		}
		plugin.requestStartPortHandle = response.GetHandle()

	}

	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) DeactivateChannel(handle uint64, channelGid string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("DeactivateChannel: (handle: %v, channel GID: %v): ", handle, channelGid)
	logDebug(logPrefix, "called")

	status, ok := plugin.channelStatuses[channelGid]
	if !ok {
		logError(logPrefix, "unknown channel GID")
		return commsshims.PLUGIN_ERROR
	}

	plugin.channelStatuses[channelGid] = commsshims.CHANNEL_UNAVAILABLE
	plugin.sdk.OnChannelStatusChanged(handle, channelGid, commsshims.CHANNEL_UNAVAILABLE, commsshims.NewChannelProperties(), commsshims.GetRACE_BLOCKING())

	if status == commsshims.CHANNEL_UNAVAILABLE {
		return commsshims.PLUGIN_OK
	}

	linkIdsToDestroy := []string{}
	for linkId, linkProps := range plugin.linkProperties {
		if linkProps.GetChannelGid() == channelGid {
			linkIdsToDestroy = append(linkIdsToDestroy, linkId)
		}
	}

	for _, linkId := range linkIdsToDestroy {
		// Calls OnLinkStatusChanged to notify SDK that links have been destroyed and call OnConnectionStatusChanged to notify all connnections in each link have been destroyed.
		plugin.DestroyLink(handle, linkId)
	}

	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) OnUserInputReceived(handle uint64, answered bool, response string) commsshims.PluginResponse {
	logPrefix := fmt.Sprintf("OnUserInputReceived: (handle: %v): ", handle)
	logDebug(logPrefix, "called")

	if handle == plugin.requestHostnameHandle {
		if answered {
			plugin.hostname = response
			logInfo(logPrefix, "using hostname ", plugin.hostname)
		} else {
			logError(logPrefix, "direct channel not available without the hostname")
			plugin.channelStatuses[DIRECT_CHANNEL_GID] = commsshims.CHANNEL_DISABLED
			channelProps := getDefaultChannelPropertiesForChannel(plugin.sdk, DIRECT_CHANNEL_GID)
			plugin.sdk.OnChannelStatusChanged(
				commsshims.GetNULL_RACE_HANDLE(),
				DIRECT_CHANNEL_GID,
				commsshims.CHANNEL_DISABLED,
				channelProps,
				commsshims.GetRACE_BLOCKING(),
			)
			// Do not continue handling input
			return commsshims.PLUGIN_OK
		}

		plugin.requestHostnameHandle = 0
	} else if handle == plugin.requestStartPortHandle {
		if answered {
			port, err := strconv.Atoi(response)
			if err != nil {
				logWarning(logPrefix, "error parsing start port, ", response)
			} else {
				plugin.nextAvailablePort = port
				logInfo(logPrefix, "using start port ", plugin.nextAvailablePort)
			}
		} else {
			logWarning(logPrefix, "no answer, using default start port")
		}

		plugin.requestStartPortHandle = 0
	} else {
		logWarning(logPrefix, "handle is not recognized")
		return commsshims.PLUGIN_ERROR
	}

	// Check if all requests have been fulfilled
	if plugin.requestHostnameHandle == 0 && plugin.requestStartPortHandle == 0 {
		plugin.channelStatuses[DIRECT_CHANNEL_GID] = commsshims.CHANNEL_AVAILABLE
		channelProps := getDefaultChannelPropertiesForChannel(plugin.sdk, DIRECT_CHANNEL_GID)
		plugin.sdk.OnChannelStatusChanged(
			commsshims.GetNULL_RACE_HANDLE(),
			DIRECT_CHANNEL_GID,
			commsshims.CHANNEL_AVAILABLE,
			channelProps,
			commsshims.GetRACE_BLOCKING(),
		)
		plugin.sdk.DisplayInfoToUser(fmt.Sprintf("%v is available", DIRECT_CHANNEL_GID), commsshims.UD_TOAST)
	}

	logDebug(logPrefix, "returned")
	return commsshims.PLUGIN_OK
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) FlushChannel(handle uint64, channelGid string, batchId uint64) commsshims.PluginResponse {
	logError("FlushChannel: plugin does not support flushing")
	return commsshims.PLUGIN_ERROR
}

func (plugin *overwrittenMethodsOnCommsPluginTwoSix) OnUserAcknowledgementReceived(handle uint64) commsshims.PluginResponse {
	logDebug("OnUserAcknowledgementReceived: called")
	return commsshims.PLUGIN_OK
}

// TODO: this wrapper function is used for convenience until a SWIG typemap is created for std::vector<std::string> to []string.
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) raceSdkReceiveEncPkgWrapper(encPkg commsshims.EncPkg, connectionIds []string) {
	connectionIdsVector := commsshims.NewStringVector()
	defer commsshims.DeleteStringVector(connectionIdsVector)
	for _, persona := range connectionIds {
		connectionIdsVector.Add(persona)
	}

	// Send EncPkg to the SDK for processing
	response := plugin.sdk.ReceiveEncPkg(encPkg, connectionIdsVector, commsshims.GetRACE_BLOCKING())

	// Handle Success/Failure
	responseStatus := response.GetStatus()
	if responseStatus != commsshims.SDK_OK {
		// TODO better handling of failure to receive EncPkg
		logError("Failed sending encPkg for connections ", connectionIdsVector.Size(), " to the SDK: ", responseStatus)
	}
}

// TODO
func (plugin *overwrittenMethodsOnCommsPluginTwoSix) connectionMonitor(connection CommsConn) {
	logInfo("connectionMonitor: called")
	defer logInfo("connectionMonitor: returned")
	connection.Receive(plugin)
	logInfo("connectionMonitor: Shutting down")
	plugin.recvChannel <- 1
}

var plugin *overwrittenMethodsOnCommsPluginTwoSix = nil

// TODO
func InitCommsPluginTwoSix(sdk uintptr) {
	logInfo("InitCommsPluginTwoSix: called")
	if plugin != nil {
		logWarning("Trying to construct a new Golang plugin when one has been created already")
		return
	}

	plugin = &overwrittenMethodsOnCommsPluginTwoSix{}
	plugin.sdk = commsshims.SwigcptrIRaceSdkComms(sdk)

	logInfo("InitCommsPluginTwoSix: returned")
}

//export CreatePluginCommsGolang
func CreatePluginCommsGolang(sdk uintptr) {
	logInfo("CreatePluginCommsGolang: called")
	InitCommsPluginTwoSix(sdk)
	logInfo("CreatePluginCommsGolang: returned")
}

//export DestroyPluginCommsGolang
func DestroyPluginCommsGolang() {
	logInfo("DestroyPluginCommsGolang: called")
	if plugin != nil {
		plugin = nil
	}
	logInfo("DestroyPluginCommsGolang: returned")
}

// For some reason, commsshims.PluginResponse, etc. are not recognized as exportable types
type PluginResponse int
type LinkType int

// Swig didn't bother to export this function, so here it is, copied straight from
// commsPluginBindingsGolang.go all its glory (or should I say... gory). We need this
// in order to properly free memory allocated by C++.
type swig_gostring struct {
	p uintptr
	n int
}

func swigCopyString(s string) string {
	p := *(*swig_gostring)(unsafe.Pointer(&s))
	r := string((*[0x7fffffff]byte)(unsafe.Pointer(p.p))[:p.n])
	commsshims.Swig_free(p.p)
	return r
}

//export PluginCommsGolangInit
func PluginCommsGolangInit(pluginConfig uintptr) PluginResponse {
	return PluginResponse(plugin.Init(commsshims.SwigcptrPluginConfig(pluginConfig)))
}

//export PluginCommsGolangShutdown
func PluginCommsGolangShutdown() PluginResponse {
	return PluginResponse(plugin.Shutdown())
}

//export PluginCommsGolangSendPackage
func PluginCommsGolangSendPackage(handle uint64, connectionId string, encPkg uintptr, timeoutTimestamp float64, batchId uint64) PluginResponse {
	return PluginResponse(plugin.SendPackage(handle, swigCopyString(connectionId), commsshims.SwigcptrEncPkg(encPkg), timeoutTimestamp, batchId))
}

//export PluginCommsGolangOpenConnection
func PluginCommsGolangOpenConnection(handle uint64, linkType LinkType, linkId string, link_hints string, send_timeout int) PluginResponse {
	return PluginResponse(plugin.OpenConnection(handle, commsshims.LinkType(linkType), swigCopyString(linkId), link_hints, send_timeout))
}

//export PluginCommsGolangCloseConnection
func PluginCommsGolangCloseConnection(handle uint64, connectionId string) PluginResponse {
	return PluginResponse(plugin.CloseConnection(handle, swigCopyString(connectionId)))
}

//export PluginCommsGolangDestroyLink
func PluginCommsGolangDestroyLink(handle uint64, linkId string) PluginResponse {
	return PluginResponse(plugin.DestroyLink(handle, swigCopyString(linkId)))
}

//export PluginCommsGolangCreateLink
func PluginCommsGolangCreateLink(handle uint64, channelGid string) PluginResponse {
	return PluginResponse(plugin.CreateLink(handle, swigCopyString(channelGid)))
}

//export PluginCommsGolangCreateLinkFromAddress
func PluginCommsGolangCreateLinkFromAddress(handle uint64, channelGid string, linkAddress string) PluginResponse {
	return PluginResponse(plugin.CreateLinkFromAddress(handle, swigCopyString(channelGid), swigCopyString(linkAddress)))
}

//export PluginCommsGolangLoadLinkAddress
func PluginCommsGolangLoadLinkAddress(handle uint64, channelGid string, linkAddress string) PluginResponse {
	return PluginResponse(plugin.LoadLinkAddress(handle, swigCopyString(channelGid), swigCopyString(linkAddress)))
}

//export PluginCommsGolangLoadLinkAddresses
func PluginCommsGolangLoadLinkAddresses(handle uint64, channelGid string, linkAddresses uintptr) PluginResponse {
	return PluginResponse(plugin.LoadLinkAddresses(handle, swigCopyString(channelGid), commsshims.SwigcptrStringVector(linkAddresses)))
}

//export PluginCommsGolangDeactivateChannel
func PluginCommsGolangDeactivateChannel(handle uint64, channelGid string) PluginResponse {
	return PluginResponse(plugin.DeactivateChannel(handle, swigCopyString(channelGid)))
}

//export PluginCommsGolangActivateChannel
func PluginCommsGolangActivateChannel(handle uint64, channelGid string, roleName string) PluginResponse {
	return PluginResponse(plugin.ActivateChannel(handle, swigCopyString(channelGid), swigCopyString(roleName)))
}

//export PluginCommsGolangOnUserInputReceived
func PluginCommsGolangOnUserInputReceived(handle uint64, answered bool, response string) PluginResponse {
	return PluginResponse(plugin.OnUserInputReceived(handle, answered, swigCopyString(response)))
}

//export PluginCommsGolangFlushChannel
func PluginCommsGolangFlushChannel(handle uint64, connId string, batchId uint64) PluginResponse {
	return PluginResponse(plugin.FlushChannel(handle, swigCopyString(connId), batchId))
}

//export PluginCommsGolangOnUserAcknowledgementReceived
func PluginCommsGolangOnUserAcknowledgementReceived(handle uint64) PluginResponse {
	return PluginResponse(plugin.OnUserAcknowledgementReceived(handle))
}

// TODO
func main() {}
