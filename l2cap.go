// TODO: Figure out about how to structure things for multiple
// OS / BLE interface configurations. Build tags? Subpackages?

package gatt

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"syscall"
)

// l2capHandler is the set of callback methods required to handle l2cap events.
type l2capHandler interface {
	readChar(c *Characteristic, maxlen int, offset int) (data []byte, status byte)
	writeChar(c *Characteristic, data []byte, noResponse bool) (status byte)
	startNotify(c *Characteristic, maxlen int)
	stopNotify(c *Characteristic)
	connected(hw net.HardwareAddr)
	disconnected(hw net.HardwareAddr)
	receivedRSSI(rssi int)
	receivedBDAddr(bdaddr string)
	// TODO: MTUChange?
	// TODO: SecurityChange?
}

// newL2cap uses s to provide l2cap access.
func newL2cap(s shim, handler l2capHandler) *l2cap {
	c := &l2cap{
		shim:    s,
		readbuf: bufio.NewReader(s),
		mtu:     23,
		handler: handler,
	}
	return c
}

type security int

const (
	securityLow = iota
	securityMed
	securityHigh
)

type l2cap struct {
	shim     shim
	readbuf  *bufio.Reader
	sendmu   sync.Mutex // serializes writes to the shim
	mtu      uint16
	handles  *handleRange
	security security
	handler  l2capHandler
	serving  bool
	quit     chan struct{}
}

func (c *l2cap) listenAndServe() error {
	if c.serving {
		return errors.New("already serving")
	}
	c.serving = true
	c.quit = make(chan struct{})
	return c.eventloop()
}

func (c *l2cap) setServices(name string, svcs []*Service) error {
	// cannot be called while serving
	if c.serving {
		return errors.New("cannot set services while serving")
	}
	c.handles = generateHandles(name, svcs, uint16(1)) // ble handles start at 1
	// log.Println("Generated handles: ", c.handles)
	return nil
}

func (c *l2cap) close() error {
	if !c.serving {
		return errors.New("not serving")
	}
	c.serving = false
	close(c.quit)
	return nil
}

func (c *l2cap) eventloop() error {
	for {
		// TODO: Check c.quit *concurrently* with other
		// blocking activities.
		select {
		case <-c.quit:
			return nil
		default:
		}

		s, err := c.readbuf.ReadString('\n')
		// log.Printf("L2CAP: Received %s", s)
		if err != nil {
			return err
		}
		f := strings.Fields(s)
		if len(f) < 2 {
			continue
		}

		// TODO: Think about concurrency here. Do we want to spawn
		// new goroutines to not block this core loop?

		switch f[0] {
		case "accept":
			hw, err := net.ParseMAC(f[1])
			if err != nil {
				return errors.New("failed to parse accepted addr " + f[1] + ": " + err.Error())
			}
			c.handler.connected(hw)
			c.mtu = 23
		case "disconnect":
			hw, err := net.ParseMAC(f[1])
			if err != nil {
				return errors.New("failed to parse disconnected addr " + f[1] + ": " + err.Error())
			}
			c.handler.disconnected(hw)
		case "rssi":
			n, err := strconv.Atoi(f[1])
			if err != nil {
				return errors.New("failed to parse rssi " + f[1] + ": " + err.Error())
			}
			c.handler.receivedRSSI(n)
		case "security":
			switch f[1] {
			case "low":
				c.security = securityLow
			case "medium":
				c.security = securityMed
			case "high":
				c.security = securityHigh
			default:
				return errors.New("unexpected security change: " + f[1])
			}
			// TODO: notify l2capHandler about security change
		case "bdaddr":
			c.handler.receivedBDAddr(f[1])
		case "hciDeviceId":
			// log.Printf("l2cap hci device: %s", f[1])
		case "data":
			req, err := hex.DecodeString(f[1])
			if err != nil {
				return err
			}
			if err = c.handleReq(req); err != nil {
				return err
			}
		}
	}
}

func (c *l2cap) disconnect() error {
	return c.shim.Signal(syscall.SIGHUP)
}

func (c *l2cap) updateRSSI() error {
	return c.shim.Signal(syscall.SIGUSR1)
}

func (c *l2cap) send(b []byte) error {
	if len(b) > int(c.mtu) {
		panic(fmt.Errorf("cannot send %x: mtu %d", b, c.mtu))
	}

	// log.Printf("L2CAP: Sending %x", b)
	c.sendmu.Lock()
	_, err := fmt.Fprintf(c.shim, "%x\n", b)
	c.sendmu.Unlock()
	return err
}

type attErr struct {
	opcode uint8
	handle uint16
	status uint8
}

// TODO: Reformulate in a way that lets the caller avoid allocs.
// Accept a []byte? Write directly to an io.Writer?
func (e attErr) Marshal() []byte {
	// little-endian encoding for handle
	return []byte{attOpError, e.opcode, byte(e.handle), byte(e.handle >> 8), e.status}
}

// handleReq dispatches a raw request from the l2cap shim
// to an appropriate handler, based on its type.
// It panics if len(b) == 0.
func (c *l2cap) handleReq(b []byte) error {
	var resp []byte

	switch reqType, req := b[0], b[1:]; reqType {
	case attOpMtuReq:
		resp = c.handleMTU(req)
	case attOpFindInfoReq:
		resp = c.handleFindInfo(req)
	case attOpFindByTypeReq:
		resp = c.handleFindByType(req)
	case attOpReadByTypeReq:
		resp = c.handleReadByType(req)
	case attOpReadReq, attOpReadBlobReq:
		resp = c.handleRead(reqType, req)
	case attOpReadByGroupReq:
		resp = c.handleReadByGroup(req)
	case attOpWriteReq, attOpWriteCmd:
		resp = c.handleWrite(reqType, req)
	case attOpReadMultiReq, attOpPrepWriteReq, attOpExecWriteReq, attOpSignedWriteCmd:
		fallthrough
	default:
		resp = attErr{opcode: reqType, handle: 0x0000, status: attEcodeReqNotSupp}.Marshal()
	}

	return c.send(resp)
}

func (c *l2cap) handleMTU(b []byte) []byte {
	c.mtu = binary.LittleEndian.Uint16(b)
	// This sanity check helps keep the response
	// writing code easier, since you don't have
	// to double-check that the response headers
	// will fit in the MTU. This is also the min
	// allowed by the BLE spec; we're just
	// enforcing it.
	if c.mtu < 23 {
		c.mtu = 23
	}
	return []byte{attOpMtuResp, b[0], b[1]}
}

func (c *l2cap) handleFindInfo(b []byte) []byte {
	start, end := readHandleRange(b)

	w := newL2capWriter(c.mtu)
	w.WriteByte(attOpFindInfoResp)
	uuidLen := -1
	for _, h := range c.handles.Subrange(start, end) {
		var uuid UUID
		switch h.typ {
		case "service":
			uuid = gattAttrPrimaryServiceUUID
		case "includedService":
			uuid = gattAttrSecondaryServiceUUID
		case "characteristic":
			uuid = gattAttrCharacteristicUUID
		case "characteristicValue", "descriptor":
			uuid = h.uuid
		default:
			continue
		}

		if uuidLen == -1 {
			uuidLen = uuid.Len()
			if uuidLen == 2 {
				w.WriteByte(0x01) // TODO: constants for 16bit vs 128bit uuid magic numbers here
			} else {
				w.WriteByte(0x02)
			}
		}
		if uuid.Len() != uuidLen {
			break
		}

		w.Chunk()
		w.WriteUint16(h.n)
		w.WriteUUID(uuid)
		if ok := w.Commit(); !ok {
			break
		}
	}

	if uuidLen == -1 {
		return attErr{opcode: attOpFindInfoReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
	}
	return w.Bytes()
}

func (c *l2cap) handleFindByType(b []byte) []byte {
	start, end := readHandleRange(b)

	if uuid := (UUID{reverse(b[4:6])}); !uuidEqual(uuid, gattAttrPrimaryServiceUUID) {
		return attErr{opcode: attOpFindByTypeReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
	}

	uuid := UUID{reverse(b[6:])}

	w := newL2capWriter(c.mtu)
	w.WriteByte(attOpFindByTypeResp)

	var wrote bool
	for _, h := range c.handles.Subrange(start, end) {
		if !h.isPrimaryService(uuid) {
			continue
		}
		w.Chunk()
		w.WriteUint16(h.startn)
		w.WriteUint16(h.endn)
		if ok := w.Commit(); !ok {
			break
		}
		wrote = true
	}

	if !wrote {
		return attErr{opcode: attOpFindByTypeReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
	}

	return w.Bytes()
}

func (c *l2cap) handleReadByType(b []byte) []byte {
	start, end := readHandleRange(b)
	uuid := UUID{reverse(b[4:])}

	// TODO: Refactor out into two extra helper handle* functions?
	if uuidEqual(uuid, gattAttrCharacteristicUUID) {
		w := newL2capWriter(c.mtu)
		w.WriteByte(attOpReadByTypeResp)
		uuidLen := -1
		for _, h := range c.handles.Subrange(start, end) {
			if h.typ != "characteristic" {
				continue
			}
			if uuidLen == -1 {
				uuidLen = h.uuid.Len()
				w.WriteByte(byte(uuidLen + 5))
			}
			if h.uuid.Len() != uuidLen {
				break
			}
			w.Chunk()
			w.WriteUint16(h.startn)
			w.WriteByte(byte(h.props))
			w.WriteUint16(h.valuen)
			w.WriteUUID(h.uuid)
			if ok := w.Commit(); !ok {
				break
			}
		}
		if uuidLen == -1 {
			return attErr{opcode: attOpReadByTypeReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
		}
		return w.Bytes()
	}

	// TODO: Refactor out into two extra helper handle* functions?
	// !bytes.Equal(uuid, gattAttrCharacteristicUUID)
	var valuen uint16
	var found bool
	var secure bool

	for _, h := range c.handles.Subrange(start, end) {
		if h.isCharacteristic(uuid) {
			valuen = h.valuen
			secure = h.secure&charRead != 0
			found = true
			break
		}
		if h.isDescriptor(uuid) {
			valuen = h.n
			secure = h.secure&charRead != 0
			found = true
			break
		}
	}

	if !found {
		return attErr{opcode: attOpReadByTypeReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
	}
	if secure && c.security > securityLow {
		return attErr{opcode: attOpReadByTypeReq, handle: start, status: attEcodeAuthentication}.Marshal()
	}

	valueh, ok := c.handles.At(valuen)
	if !ok {
		// This can only happen (I think) if we've done
		// a bad job constructing our handles.
		panic(fmt.Errorf("bad value handle reading %x: %v\n\nHandles: %#v", uuid, valuen, c.handles))
	}
	w := newL2capWriter(c.mtu)
	datalen := w.Writeable(4, valueh.value)
	w.WriteByte(attOpReadByTypeResp)
	w.WriteByte(byte(datalen + 2))
	w.WriteUint16(valuen)
	w.WriteFit(valueh.value)

	return w.Bytes()
}

func (c *l2cap) handleRead(reqType byte, b []byte) []byte {
	valuen := binary.LittleEndian.Uint16(b)
	var offset uint16
	if reqType == attOpReadBlobReq {
		offset = binary.LittleEndian.Uint16(b[2:])
	}
	respType := attRespFor[reqType]
	_ = offset

	h, ok := c.handles.At(valuen)
	if !ok {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeInvalidHandle}.Marshal()
	}

	w := newL2capWriter(c.mtu)
	w.WriteByte(respType)
	w.Chunk()

	switch h.typ {
	case "service", "includedService":
		w.WriteUUID(h.uuid)
	case "characteristic":
		w.WriteByte(byte(h.props))
		w.WriteUint16(h.valuen)
		w.WriteUUID(h.uuid)
	case "characteristicValue", "descriptor":
		valueh := h
		if h.typ == "characteristicValue" {
			vh, ok := c.handles.At(valuen - 1) // TODO: Store a cross-reference explicitly instead of this -1 nonsense.
			if !ok {
				panic(fmt.Errorf("invalid handle reference reading characteristicValue handle %d: %v\n\nHandles: %#v", valuen-1, c.handles))
			}
			valueh = vh
		}
		if valueh.props&charRead == 0 {
			return attErr{opcode: reqType, handle: valuen, status: attEcodeReadNotPerm}.Marshal()
		}
		if valueh.secure&charRead != 0 && c.security > securityLow {
			return attErr{opcode: reqType, handle: valuen, status: attEcodeAuthentication}.Marshal()
		}
		if h.value != nil {
			w.WriteFit(h.value)
		} else {
			// Ask server for data
			char := valueh.attr.(*Characteristic) // TODO: Rethink attr being interface{}
			data, status := c.handler.readChar(char, int(c.mtu-1), int(offset))
			if status != StatusSuccess {
				return attErr{opcode: reqType, handle: valuen, status: byte(status)}.Marshal()
			}
			w.WriteFit(data)
			offset = 0 // the handler has already adjusted for the offset
		}
	default:
		// Shouldn't happen?
		return attErr{opcode: reqType, handle: valuen, status: attEcodeInvalidHandle}.Marshal()
	}

	if ok := w.ChunkSeek(offset); !ok {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeInvalidOffset}.Marshal()
	}

	w.CommitFit()
	return w.Bytes()
}

func (c *l2cap) handleReadByGroup(b []byte) []byte {
	start, end := readHandleRange(b)
	uuid := UUID{reverse(b[4:])}

	var typ string
	switch {
	case uuidEqual(uuid, gattAttrPrimaryServiceUUID):
		typ = "service"
	case uuidEqual(uuid, gattAttrIncludeUUID):
		typ = "includedService"
	default:
		return attErr{opcode: attOpReadByGroupReq, handle: start, status: attEcodeUnsuppGrpType}.Marshal()
	}

	w := newL2capWriter(c.mtu)
	w.WriteByte(attOpReadByGroupResp)
	uuidLen := -1
	for _, h := range c.handles.Subrange(start, end) {
		if h.typ != typ {
			continue
		}
		if uuidLen == -1 {
			uuidLen = h.uuid.Len()
			w.WriteByte(byte(uuidLen + 4))
		}
		if uuidLen != h.uuid.Len() {
			break
		}
		w.Chunk()
		w.WriteUint16(h.startn)
		w.WriteUint16(h.endn)
		w.WriteUUID(h.uuid)
		if ok := w.Commit(); !ok {
			break
		}
	}
	if uuidLen == -1 {
		return attErr{opcode: attOpReadByGroupReq, handle: start, status: attEcodeAttrNotFound}.Marshal()
	}

	return w.Bytes()
}

func (c *l2cap) handleWrite(reqType byte, b []byte) []byte {
	valuen := binary.LittleEndian.Uint16(b)
	data := b[2:]

	h, ok := c.handles.At(valuen)
	if !ok {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeInvalidHandle}.Marshal()
	}

	if h.typ == "characteristicValue" {
		vh, ok := c.handles.At(valuen - 1) // TODO: Clean this up somehow by storing a better ref explicitly.
		if !ok {
			panic(fmt.Errorf("invalid handle reference writing characteristicValue handle %d: %v\n\nHandles: %#v", valuen-1, c.handles))
		}
		h = vh
	}

	noResp := reqType == attOpWriteCmd
	charFlag := uint(charWrite)
	if noResp {
		charFlag = charWriteNR
	}

	if h.props&charFlag == 0 {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeWriteNotPerm}.Marshal()
	}
	if h.secure&charFlag == 0 && c.security > securityLow {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeAuthentication}.Marshal()
	}

	if h.typ != "descriptor" && !uuidEqual(h.uuid, gattAttrClientCharacteristicConfigUUID) {
		// Regular write, not CCC
		result := c.handler.writeChar(h.attr.(*Characteristic), data, noResp)
		if noResp {
			return nil
		}
		if result != StatusSuccess {
			return attErr{opcode: reqType, handle: valuen, status: byte(result)}.Marshal()
		}
		return []byte{attOpWriteResp}
	}

	// CCC/descriptor write
	if len(data) != 2 {
		return attErr{opcode: reqType, handle: valuen, status: attEcodeInvalAttrValueLen}.Marshal()
	}

	ccc := binary.LittleEndian.Uint16(data)
	char := h.attr.(*Characteristic)
	h.value = data

	if ccc&gattCCCNotifyFlag == 0 {
		// TODO: Suppress these calls if the notification state hasn't actually changed
		c.handler.stopNotify(char)
		if noResp {
			return nil
		}
		return []byte{attOpWriteResp}
	}

	c.handler.startNotify(char, int(c.mtu-3))
	if noResp {
		return nil
	}
	return []byte{attOpWriteResp}
}

func (c *l2cap) sendNotification(char *Characteristic, data []byte) error {
	w := newL2capWriter(c.mtu)
	w.WriteByte(attOpHandleNotify)
	w.WriteUint16(char.valuen)
	w.WriteFit(data)
	b := w.Bytes()
	return c.send(b)
}

func readHandleRange(b []byte) (start, end uint16) {
	return binary.LittleEndian.Uint16(b), binary.LittleEndian.Uint16(b[2:])
}
