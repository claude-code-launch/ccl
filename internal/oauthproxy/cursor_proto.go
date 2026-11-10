package oauthproxy

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// Cursor AgentService field numbers come from Cursor's TypeScript agent.proto
// (the same numbers CLIProxy's current FileDescriptor uses). They are owned
// here as constants so the runtime never depends on a copied descriptor.

const (
	cursorACMRunRequest        = 1
	cursorACMExecClient        = 2
	cursorACMKvClient          = 3
	cursorACMClientHeartbeat   = 7
	cursorASMInteractionUpdate = 1
	cursorASMExecServer        = 2
	cursorASMCheckpoint        = 3
	cursorASMKvServer          = 4

	cursorARRConversationState = 1
	cursorARRAction            = 2
	cursorARRModelDetails      = 3
	cursorARRMcpTools          = 4
	cursorARRConversationID    = 5

	cursorCSSRootPromptMessagesJSON = 1

	cursorCAUserMessageAction = 1
	cursorUMAUserMessage      = 1
	cursorUMText              = 1
	cursorUMMessageID         = 2
	cursorUMSelectedContext   = 3
	cursorSCSelectedImages    = 1
	cursorSIUUID              = 2
	cursorSIMimeType          = 7
	cursorSIData              = 8

	cursorMDModelID         = 1
	cursorMDThinkingDetails = 2
	cursorMDDisplayModelID  = 3
	cursorMDDisplayName     = 4

	cursorMTMcpTools            = 1
	cursorMTDName               = 1
	cursorMTDDescription        = 2
	cursorMTDInputSchema        = 3
	cursorMTDProviderIdentifier = 4
	cursorMTDToolName           = 5
	cursorMCPProviderIdentifier = "ccl"

	cursorIUTextDelta         = 1
	cursorIUThinkingDelta     = 4
	cursorIUThinkingCompleted = 5
	cursorIUTokenDelta        = 8
	cursorIUHeartbeat         = 13
	cursorIUTurnEnded         = 14
	cursorTDUText             = 1

	cursorKSMID        = 1
	cursorKSMGetBlob   = 2
	cursorKSMSetBlob   = 3
	cursorGBABlobID    = 1
	cursorSBABlobID    = 1
	cursorSBABlobData  = 2
	cursorKCMID        = 1
	cursorKCMGetResult = 2
	cursorKCMSetResult = 3
	cursorGBRBlobData  = 1

	cursorESMID               = 1
	cursorESMShell            = 2
	cursorESMWrite            = 3
	cursorESMDelete           = 4
	cursorESMGrep             = 5
	cursorESMRead             = 7
	cursorESMLs               = 8
	cursorESMDiagnostics      = 9
	cursorESMRequestContext   = 10
	cursorESMMcpArgs          = 11
	cursorESMShellStream      = 14
	cursorESMExecID           = 15
	cursorESMBgShellSpawn     = 16
	cursorESMFetch            = 20
	cursorESMWriteShellStdin  = 23
	cursorMCAName             = 1
	cursorMCAArgs             = 2
	cursorMCAToolCallID       = 3
	cursorMCAToolName         = 5
	cursorRCRSuccess          = 1
	cursorRCSRequestContext   = 1
	cursorRCTools             = 7
	cursorMCRSuccess          = 1
	cursorMCRError            = 2
	cursorMCSContent          = 1
	cursorMCSIsError          = 2
	cursorMTRCIText           = 1
	cursorMTCText             = 1
	cursorMCEError            = 1
	cursorRRRejected          = 3
	cursorSRRejected          = 5
	cursorWRRejected          = 5
	cursorDRRejected          = 3
	cursorLRRejected          = 3
	cursorGRError             = 2
	cursorFRError             = 2
	cursorBSSRRejected        = 2
	cursorWSSRError           = 2
	cursorREJPath             = 1
	cursorREJReason           = 2
	cursorSREJCommand         = 1
	cursorSREJWorkingDir      = 2
	cursorSREJReason          = 3
	cursorGERRError           = 1
	cursorFERRURL             = 1
	cursorFERRError           = 2
	cursorRAPath              = 1
	cursorWAPath              = 1
	cursorDAPath              = 1
	cursorLAPath              = 1
	cursorSHACommand          = 1
	cursorSHAWorkingDirectory = 2
	cursorFAURL               = 1

	cursorConnectEndStream      byte = 0x02
	cursorConnectCompression    byte = 0x01
	cursorConnectHeaderSize          = 5
	cursorMaxConnectFrame            = 16 << 20
	cursorProtobufValueMaxDepth      = 32

	cursorBuiltinRejectReason = "Tool not available in this environment. Use the MCP tools provided instead."
	cursorHistoryPreamble     = "The above is the previous conversation context.\nContinue from the conversation above."
)

const (
	cursorWireVarint  = 0
	cursorWireFixed64 = 1
	cursorWireBytes   = 2
	cursorWireFixed32 = 5
)

type cursorImage struct {
	MIME string
	Data []byte
}

type cursorMCPTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

type cursorRunParams struct {
	ModelID        string
	SystemPrompt   string
	UserText       string
	MessageID      string
	ConversationID string
	Images         []cursorImage
	Tools          []cursorMCPTool
	Thinking       bool
	Blobs          map[string][]byte
}

type cursorToolResult struct {
	ToolUseID string
	Text      string
	IsError   bool
}

type cursorDecoded struct {
	Kind       string
	Text       string
	TokenDelta int64
	KvID       uint32
	BlobID     []byte
	BlobData   []byte
	ExecMsgID  uint32
	ExecID     string
	MCPName    string
	MCPCallID  string
	MCPArgs    map[string][]byte
	Path       string
	Command    string
	WorkDir    string
	URL        string
	ExecField  int
}

type cursorConnectError struct {
	Code    string
	Message string
	Status  int
}

func (e *cursorConnectError) Error() string {
	if e.Message == "" {
		return "cursor connect error " + e.Code
	}
	return e.Message
}

// upstreamStatus exposes the HTTP-equivalent status to retryUpstream so a
// Connect trailer error takes the same 429/5xx fast-retry path as a raw
// upstream response.
func (e *cursorConnectError) upstreamStatus() int {
	if e.Status != 0 {
		return e.Status
	}
	return cursorConnectStatus(e.Code)
}

func cursorAppendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func cursorAppendTag(buf []byte, field, wire int) []byte {
	return cursorAppendVarint(buf, uint64(field<<3|wire))
}

func cursorAppendBytes(buf []byte, field int, v []byte) []byte {
	buf = cursorAppendTag(buf, field, cursorWireBytes)
	buf = cursorAppendVarint(buf, uint64(len(v)))
	return append(buf, v...)
}

func cursorAppendString(buf []byte, field int, v string) []byte {
	if v == "" {
		return buf
	}
	return cursorAppendBytes(buf, field, []byte(v))
}

func cursorAppendStringRaw(buf []byte, field int, v string) []byte {
	return cursorAppendBytes(buf, field, []byte(v))
}

func cursorAppendUint32(buf []byte, field int, v uint32) []byte {
	buf = cursorAppendTag(buf, field, cursorWireVarint)
	return cursorAppendVarint(buf, uint64(v))
}

func cursorAppendBool(buf []byte, field int, v bool) []byte {
	if !v {
		return buf
	}
	buf = cursorAppendTag(buf, field, cursorWireVarint)
	return cursorAppendVarint(buf, 1)
}

func cursorAppendFixed64(buf []byte, field int, v uint64) []byte {
	buf = cursorAppendTag(buf, field, cursorWireFixed64)
	var raw [8]byte
	binary.LittleEndian.PutUint64(raw[:], v)
	return append(buf, raw[:]...)
}

func cursorConsumeVarint(data []byte) (uint64, int) {
	var value uint64
	var shift uint
	for i := 0; i < len(data) && i < 10; i++ {
		b := data[i]
		value |= uint64(b&0x7f) << shift
		if b < 0x80 {
			return value, i + 1
		}
		shift += 7
	}
	return 0, -1
}

func cursorConsumeTag(data []byte) (num, typ, n int) {
	value, n := cursorConsumeVarint(data)
	if n < 0 {
		return 0, 0, -1
	}
	return int(value >> 3), int(value & 7), n
}

func cursorConsumeBytes(data []byte) ([]byte, int) {
	length, n := cursorConsumeVarint(data)
	if n < 0 {
		return nil, -1
	}
	if uint64(len(data)-n) < length {
		return nil, -1
	}
	end := n + int(length)
	return data[n:end], end
}

func cursorSkipField(typ int, data []byte) int {
	switch typ {
	case cursorWireVarint:
		_, n := cursorConsumeVarint(data)
		return n
	case cursorWireFixed64:
		if len(data) < 8 {
			return -1
		}
		return 8
	case cursorWireBytes:
		_, n := cursorConsumeBytes(data)
		return n
	case cursorWireFixed32:
		if len(data) < 4 {
			return -1
		}
		return 4
	default:
		return -1
	}
}

func cursorFrameConnect(payload []byte, flags byte) []byte {
	frame := make([]byte, cursorConnectHeaderSize+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame
}

func cursorParseConnectFrame(buf []byte) (flags byte, payload []byte, consumed int, ok bool) {
	if len(buf) < cursorConnectHeaderSize {
		return 0, nil, 0, false
	}
	flags = buf[0]
	length := binary.BigEndian.Uint32(buf[1:5])
	if length > cursorMaxConnectFrame {
		return 0, nil, 0, false
	}
	total := cursorConnectHeaderSize + int(length)
	if len(buf) < total {
		return 0, nil, 0, false
	}
	return flags, buf[5:total], total, true
}

func cursorParseEndStream(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var trailer struct {
		Error *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(data, &trailer); err != nil {
		return fmt.Errorf("parse Cursor connect trailer: %w", err)
	}
	if trailer.Error == nil {
		return nil
	}
	code := strings.TrimSpace(trailer.Error.Code)
	if code == "" {
		code = "unknown"
	}
	msg := strings.TrimSpace(trailer.Error.Message)
	if msg == "" {
		msg = "Unknown error"
	}
	return &cursorConnectError{Code: code, Message: msg, Status: cursorConnectStatus(code)}
}

func cursorConnectStatus(code string) int {
	switch code {
	case "resource_exhausted":
		return 429
	case "unauthenticated":
		return 401
	case "permission_denied":
		return 403
	case "unavailable":
		return 503
	case "internal":
		return 500
	default:
		return 502
	}
}

func cursorStripConnect(body []byte) []byte {
	if len(body) < cursorConnectHeaderSize {
		return body
	}
	flags, payload, consumed, ok := cursorParseConnectFrame(body)
	if !ok || flags != 0 || consumed > len(body) {
		return body
	}
	return payload
}

func encodeCursorHeartbeat() []byte {
	return cursorAppendBytes(nil, cursorACMClientHeartbeat, nil)
}

func encodeCursorRunRequest(p cursorRunParams) []byte {
	if p.Blobs == nil {
		p.Blobs = make(map[string][]byte)
	}
	var css []byte
	if strings.TrimSpace(p.SystemPrompt) != "" {
		systemJSON, _ := json.Marshal(map[string]string{"role": "system", "content": p.SystemPrompt})
		sum := sha256SumBytes(systemJSON)
		p.Blobs[fmt.Sprintf("%x", sum)] = systemJSON
		css = cursorAppendBytes(css, cursorCSSRootPromptMessagesJSON, sum)
	}
	user := cursorAppendString(nil, cursorUMText, p.UserText)
	user = cursorAppendString(user, cursorUMMessageID, p.MessageID)
	if len(p.Images) > 0 {
		var images []byte
		for _, image := range p.Images {
			item := cursorAppendString(nil, cursorSIUUID, uuidString())
			item = cursorAppendString(item, cursorSIMimeType, image.MIME)
			item = cursorAppendBytes(item, cursorSIData, image.Data)
			images = cursorAppendBytes(images, cursorSCSelectedImages, item)
		}
		user = cursorAppendBytes(user, cursorUMSelectedContext, images)
	}
	action := cursorAppendBytes(nil, cursorCAUserMessageAction, cursorAppendBytes(nil, cursorUMAUserMessage, user))
	model := cursorAppendString(nil, cursorMDModelID, p.ModelID)
	if p.Thinking {
		model = cursorAppendBytes(model, cursorMDThinkingDetails, nil)
	}
	model = cursorAppendString(model, cursorMDDisplayModelID, p.ModelID)
	model = cursorAppendString(model, cursorMDDisplayName, p.ModelID)
	arr := cursorAppendBytes(nil, cursorARRConversationState, css)
	arr = cursorAppendBytes(arr, cursorARRAction, action)
	arr = cursorAppendBytes(arr, cursorARRModelDetails, model)
	if encoded := encodeCursorMcpTools(p.Tools); len(encoded) > 0 {
		arr = cursorAppendBytes(arr, cursorARRMcpTools, encoded)
	}
	arr = cursorAppendString(arr, cursorARRConversationID, p.ConversationID)
	return cursorAppendBytes(nil, cursorACMRunRequest, arr)
}

func encodeCursorMcpTools(tools []cursorMCPTool) []byte {
	if len(tools) == 0 {
		return nil
	}
	var body []byte
	for _, tool := range tools {
		body = cursorAppendBytes(body, cursorMTMcpTools, encodeCursorMcpToolDef(tool))
	}
	return body
}

func encodeCursorMcpToolDef(tool cursorMCPTool) []byte {
	msg := cursorAppendString(nil, cursorMTDName, tool.Name)
	msg = cursorAppendString(msg, cursorMTDDescription, tool.Description)
	if len(bytesTrimSpace(tool.InputSchema)) > 0 {
		msg = cursorAppendBytes(msg, cursorMTDInputSchema, cursorJSONToProtobufValue(tool.InputSchema))
	}
	msg = cursorAppendString(msg, cursorMTDProviderIdentifier, cursorMCPProviderIdentifier)
	return cursorAppendString(msg, cursorMTDToolName, tool.Name)
}

func encodeCursorKvGetResult(id uint32, data []byte) []byte {
	var result []byte
	if data != nil {
		result = cursorAppendBytes(nil, cursorGBRBlobData, data)
	}
	msg := cursorAppendUint32(nil, cursorKCMID, id)
	msg = cursorAppendBytes(msg, cursorKCMGetResult, result)
	return cursorAppendBytes(nil, cursorACMKvClient, msg)
}

func encodeCursorKvSetResult(id uint32) []byte {
	msg := cursorAppendUint32(nil, cursorKCMID, id)
	msg = cursorAppendBytes(msg, cursorKCMSetResult, nil)
	return cursorAppendBytes(nil, cursorACMKvClient, msg)
}

func encodeCursorExecClient(id uint32, execID string, resultField int, result []byte) []byte {
	msg := cursorAppendUint32(nil, cursorESMID, id)
	msg = cursorAppendBytes(msg, resultField, result)
	msg = cursorAppendStringRaw(msg, cursorESMExecID, execID)
	return cursorAppendBytes(nil, cursorACMExecClient, msg)
}

func encodeCursorExecMcpResult(id uint32, execID, text string, isError bool) []byte {
	item := cursorAppendBytes(nil, cursorMTRCIText, cursorAppendString(nil, cursorMTCText, text))
	success := cursorAppendBytes(nil, cursorMCSContent, item)
	success = cursorAppendBool(success, cursorMCSIsError, isError)
	result := cursorAppendBytes(nil, cursorMCRSuccess, success)
	return encodeCursorExecClient(id, execID, cursorESMMcpArgs, result)
}

func encodeCursorRequestContext(id uint32, execID string, tools []cursorMCPTool) []byte {
	var rc []byte
	for _, tool := range tools {
		rc = cursorAppendBytes(rc, cursorRCTools, encodeCursorMcpToolDef(tool))
	}
	success := cursorAppendBytes(nil, cursorRCSRequestContext, rc)
	result := cursorAppendBytes(nil, cursorRCRSuccess, success)
	return encodeCursorExecClient(id, execID, cursorESMRequestContext, result)
}

func encodeCursorExecEmpty(id uint32, execID string, field int) []byte {
	return encodeCursorExecClient(id, execID, field, nil)
}

func encodeCursorRejectPath(id uint32, execID string, resultField, rejectedField int, path string) []byte {
	rejected := cursorAppendString(nil, cursorREJPath, path)
	rejected = cursorAppendString(rejected, cursorREJReason, cursorBuiltinRejectReason)
	return encodeCursorExecClient(id, execID, resultField, cursorAppendBytes(nil, rejectedField, rejected))
}

func encodeCursorRejectShell(id uint32, execID, command, workDir string) []byte {
	rejected := cursorAppendString(nil, cursorSREJCommand, command)
	rejected = cursorAppendString(rejected, cursorSREJWorkingDir, workDir)
	rejected = cursorAppendString(rejected, cursorSREJReason, cursorBuiltinRejectReason)
	return encodeCursorExecClient(id, execID, cursorESMShell, cursorAppendBytes(nil, cursorSRRejected, rejected))
}

func encodeCursorRejectGrep(id uint32, execID string) []byte {
	errMsg := cursorAppendString(nil, cursorGERRError, cursorBuiltinRejectReason)
	return encodeCursorExecClient(id, execID, cursorESMGrep, cursorAppendBytes(nil, cursorGRError, errMsg))
}

func encodeCursorRejectFetch(id uint32, execID, url string) []byte {
	errMsg := cursorAppendString(nil, cursorFERRURL, url)
	errMsg = cursorAppendString(errMsg, cursorFERRError, cursorBuiltinRejectReason)
	return encodeCursorExecClient(id, execID, cursorESMFetch, cursorAppendBytes(nil, cursorFRError, errMsg))
}

func encodeCursorRejectExec(decoded cursorDecoded) []byte {
	switch decoded.ExecField {
	case cursorESMShell, cursorESMShellStream:
		return encodeCursorRejectShell(decoded.ExecMsgID, decoded.ExecID, decoded.Command, decoded.WorkDir)
	case cursorESMWrite:
		return encodeCursorRejectPath(decoded.ExecMsgID, decoded.ExecID, cursorESMWrite, cursorWRRejected, decoded.Path)
	case cursorESMDelete:
		return encodeCursorRejectPath(decoded.ExecMsgID, decoded.ExecID, cursorESMDelete, cursorDRRejected, decoded.Path)
	case cursorESMRead:
		return encodeCursorRejectPath(decoded.ExecMsgID, decoded.ExecID, cursorESMRead, cursorRRRejected, decoded.Path)
	case cursorESMLs:
		return encodeCursorRejectPath(decoded.ExecMsgID, decoded.ExecID, cursorESMLs, cursorLRRejected, decoded.Path)
	case cursorESMGrep:
		return encodeCursorRejectGrep(decoded.ExecMsgID, decoded.ExecID)
	case cursorESMFetch:
		return encodeCursorRejectFetch(decoded.ExecMsgID, decoded.ExecID, decoded.URL)
	case cursorESMBgShellSpawn:
		rejected := cursorAppendString(nil, cursorREJReason, cursorBuiltinRejectReason)
		return encodeCursorExecClient(decoded.ExecMsgID, decoded.ExecID, cursorESMBgShellSpawn, cursorAppendBytes(nil, cursorBSSRRejected, rejected))
	case cursorESMWriteShellStdin:
		errMsg := cursorAppendString(nil, cursorGERRError, cursorBuiltinRejectReason)
		return encodeCursorExecClient(decoded.ExecMsgID, decoded.ExecID, cursorESMWriteShellStdin, cursorAppendBytes(nil, cursorWSSRError, errMsg))
	case cursorESMDiagnostics:
		return encodeCursorExecEmpty(decoded.ExecMsgID, decoded.ExecID, cursorESMDiagnostics)
	default:
		if decoded.ExecField > 0 {
			return encodeCursorExecEmpty(decoded.ExecMsgID, decoded.ExecID, decoded.ExecField)
		}
		return encodeCursorExecEmpty(decoded.ExecMsgID, decoded.ExecID, cursorESMDiagnostics)
	}
}

func decodeCursorASM(data []byte) []cursorDecoded {
	var messages []cursorDecoded
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ != cursorWireBytes {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				break
			}
			data = data[skip:]
			continue
		}
		val, n := cursorConsumeBytes(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch num {
		case cursorASMInteractionUpdate:
			messages = append(messages, decodeCursorInteraction(val)...)
		case cursorASMExecServer:
			messages = append(messages, decodeCursorExec(val))
		case cursorASMKvServer:
			messages = append(messages, decodeCursorKV(val))
		case cursorASMCheckpoint:
			messages = append(messages, cursorDecoded{Kind: "checkpoint", BlobData: append([]byte(nil), val...)})
		}
	}
	return messages
}

func decodeCursorInteraction(data []byte) []cursorDecoded {
	var messages []cursorDecoded
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch {
		case typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return messages
			}
			data = data[n:]
			switch num {
			case cursorIUTextDelta:
				messages = append(messages, cursorDecoded{Kind: "text", Text: cursorInnerString(val, cursorTDUText)})
			case cursorIUThinkingDelta:
				messages = append(messages, cursorDecoded{Kind: "thinking", Text: cursorInnerString(val, cursorTDUText)})
			case cursorIUThinkingCompleted:
				messages = append(messages, cursorDecoded{Kind: "thinking_done"})
			case cursorIUTokenDelta:
				messages = append(messages, cursorDecoded{Kind: "token", TokenDelta: cursorInnerVarint(val, 1)})
			case cursorIUHeartbeat:
				messages = append(messages, cursorDecoded{Kind: "heartbeat"})
			case cursorIUTurnEnded:
				messages = append(messages, cursorDecoded{Kind: "turn_end"})
			}
		case typ == cursorWireVarint:
			value, n := cursorConsumeVarint(data)
			if n < 0 {
				return messages
			}
			data = data[n:]
			if num == cursorIUTokenDelta {
				messages = append(messages, cursorDecoded{Kind: "token", TokenDelta: int64(value)})
			}
		default:
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return messages
			}
			data = data[skip:]
		}
	}
	return messages
}

func decodeCursorKV(data []byte) cursorDecoded {
	msg := cursorDecoded{Kind: "kv_unknown"}
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch {
		case typ == cursorWireVarint && num == cursorKSMID:
			value, n := cursorConsumeVarint(data)
			if n < 0 {
				return msg
			}
			msg.KvID = uint32(value)
			data = data[n:]
		case typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return msg
			}
			data = data[n:]
			switch num {
			case cursorKSMGetBlob:
				msg.Kind = "kv_get"
				msg.BlobID = cursorInnerBytes(val, cursorGBABlobID)
			case cursorKSMSetBlob:
				msg.Kind = "kv_set"
				msg.BlobID = cursorInnerBytes(val, cursorSBABlobID)
				msg.BlobData = cursorInnerBytes(val, cursorSBABlobData)
			}
		default:
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return msg
			}
			data = data[skip:]
		}
	}
	return msg
}

func decodeCursorExec(data []byte) cursorDecoded {
	msg := cursorDecoded{Kind: "exec_other"}
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch {
		case typ == cursorWireVarint && num == cursorESMID:
			value, n := cursorConsumeVarint(data)
			if n < 0 {
				return msg
			}
			msg.ExecMsgID = uint32(value)
			data = data[n:]
		case typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return msg
			}
			data = data[n:]
			switch num {
			case cursorESMExecID:
				msg.ExecID = string(val)
			case cursorESMMcpArgs:
				msg.Kind = "exec_mcp"
				msg.ExecField = num
				decodeCursorMcpArgs(val, &msg)
			case cursorESMRequestContext:
				msg.Kind = "exec_context"
				msg.ExecField = num
			case cursorESMShell, cursorESMShellStream:
				msg.Kind = "exec_shell"
				msg.ExecField = num
				msg.Command = cursorInnerString(val, cursorSHACommand)
				msg.WorkDir = cursorInnerString(val, cursorSHAWorkingDirectory)
			case cursorESMWrite:
				msg.Kind = "exec_write"
				msg.ExecField = num
				msg.Path = cursorInnerString(val, cursorWAPath)
			case cursorESMDelete:
				msg.Kind = "exec_delete"
				msg.ExecField = num
				msg.Path = cursorInnerString(val, cursorDAPath)
			case cursorESMRead:
				msg.Kind = "exec_read"
				msg.ExecField = num
				msg.Path = cursorInnerString(val, cursorRAPath)
			case cursorESMLs:
				msg.Kind = "exec_ls"
				msg.ExecField = num
				msg.Path = cursorInnerString(val, cursorLAPath)
			case cursorESMGrep:
				msg.Kind = "exec_grep"
				msg.ExecField = num
			case cursorESMFetch:
				msg.Kind = "exec_fetch"
				msg.ExecField = num
				msg.URL = cursorInnerString(val, cursorFAURL)
			case cursorESMDiagnostics:
				msg.Kind = "exec_diagnostics"
				msg.ExecField = num
			case cursorESMBgShellSpawn:
				msg.Kind = "exec_bgshell"
				msg.ExecField = num
			case cursorESMWriteShellStdin:
				msg.Kind = "exec_stdin"
				msg.ExecField = num
			default:
				msg.Kind = "exec_other"
				msg.ExecField = num
			}
		default:
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return msg
			}
			data = data[skip:]
		}
	}
	return msg
}

func decodeCursorMcpArgs(data []byte, msg *cursorDecoded) {
	msg.MCPArgs = make(map[string][]byte)
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			return
		}
		data = data[n:]
		if typ != cursorWireBytes {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return
			}
			data = data[skip:]
			continue
		}
		val, n := cursorConsumeBytes(data)
		if n < 0 {
			return
		}
		data = data[n:]
		switch num {
		case cursorMCAName:
			msg.MCPName = string(val)
		case cursorMCAToolCallID:
			msg.MCPCallID = string(val)
		case cursorMCAToolName:
			if msg.MCPName == "" {
				msg.MCPName = string(val)
			}
		case cursorMCAArgs:
			key, value := cursorMapEntry(val)
			if key != "" {
				msg.MCPArgs[key] = value
			}
		}
	}
}

func cursorMapEntry(data []byte) (string, []byte) {
	var key string
	var value []byte
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ != cursorWireBytes {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				break
			}
			data = data[skip:]
			continue
		}
		val, n := cursorConsumeBytes(data)
		if n < 0 {
			break
		}
		data = data[n:]
		switch num {
		case 1:
			key = string(val)
		case 2:
			value = append([]byte(nil), val...)
		}
	}
	return key, value
}

func cursorInnerString(data []byte, field int) string {
	return string(cursorInnerBytes(data, field))
}

func cursorInnerBytes(data []byte, field int) []byte {
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		if typ != cursorWireBytes {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return nil
			}
			data = data[skip:]
			continue
		}
		val, n := cursorConsumeBytes(data)
		if n < 0 {
			return nil
		}
		data = data[n:]
		if num == field {
			return append([]byte(nil), val...)
		}
	}
	return nil
}

func cursorInnerVarint(data []byte, field int) int64 {
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			return 0
		}
		data = data[n:]
		if typ != cursorWireVarint {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return 0
			}
			data = data[skip:]
			continue
		}
		value, n := cursorConsumeVarint(data)
		if n < 0 {
			return 0
		}
		data = data[n:]
		if num == field {
			return int64(value)
		}
	}
	return 0
}

func cursorJSONToProtobufValue(raw json.RawMessage) []byte {
	return cursorJSONToProtobufValueDepth(raw, 0)
}

func cursorJSONToProtobufValueDepth(raw json.RawMessage, depth int) []byte {
	if depth > cursorProtobufValueMaxDepth {
		return cursorAppendString(nil, 3, string(raw))
	}
	raw = bytesTrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		buf := cursorAppendTag(nil, 1, cursorWireVarint)
		return cursorAppendVarint(buf, 0)
	}
	switch raw[0] {
	case '"':
		var text string
		if json.Unmarshal(raw, &text) != nil {
			return cursorAppendString(nil, 3, string(raw))
		}
		return cursorAppendString(nil, 3, text)
	case 't', 'f':
		var flag bool
		if json.Unmarshal(raw, &flag) != nil {
			return cursorAppendString(nil, 3, string(raw))
		}
		buf := cursorAppendTag(nil, 4, cursorWireVarint)
		if flag {
			return cursorAppendVarint(buf, 1)
		}
		return cursorAppendVarint(buf, 0)
	case '{':
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil {
			return cursorAppendString(nil, 3, string(raw))
		}
		var fields []byte
		for key, value := range object {
			entry := cursorAppendString(nil, 1, key)
			entry = cursorAppendBytes(entry, 2, cursorJSONToProtobufValueDepth(value, depth+1))
			fields = cursorAppendBytes(fields, 1, entry)
		}
		return cursorAppendBytes(nil, 5, fields)
	case '[':
		var list []json.RawMessage
		if json.Unmarshal(raw, &list) != nil {
			return cursorAppendString(nil, 3, string(raw))
		}
		var values []byte
		for _, item := range list {
			values = cursorAppendBytes(values, 1, cursorJSONToProtobufValueDepth(item, depth+1))
		}
		return cursorAppendBytes(nil, 6, values)
	default:
		var number float64
		if json.Unmarshal(raw, &number) != nil {
			return cursorAppendString(nil, 3, string(raw))
		}
		return cursorAppendFixed64(nil, 2, math.Float64bits(number))
	}
}

func cursorProtobufValueToJSON(data []byte) json.RawMessage {
	raw, _ := cursorProtobufValueToJSONDepth(data, 0)
	if len(raw) == 0 {
		return json.RawMessage("null")
	}
	return raw
}

func cursorProtobufValueToJSONDepth(data []byte, depth int) (json.RawMessage, bool) {
	if depth > cursorProtobufValueMaxDepth {
		return json.RawMessage("null"), false
	}
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			return json.RawMessage("null"), false
		}
		data = data[n:]
		switch {
		case num == 1 && typ == cursorWireVarint:
			return json.RawMessage("null"), true
		case num == 2 && typ == cursorWireFixed64:
			if len(data) < 8 {
				return json.RawMessage("null"), false
			}
			bits := binary.LittleEndian.Uint64(data[:8])
			raw, _ := json.Marshal(math.Float64frombits(bits))
			return raw, true
		case num == 3 && typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return json.RawMessage("null"), false
			}
			raw, _ := json.Marshal(string(val))
			return raw, true
		case num == 4 && typ == cursorWireVarint:
			value, n := cursorConsumeVarint(data)
			if n < 0 {
				return json.RawMessage("null"), false
			}
			if value != 0 {
				return json.RawMessage("true"), true
			}
			return json.RawMessage("false"), true
		case num == 5 && typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return json.RawMessage("null"), false
			}
			return cursorProtobufStructToJSON(val, depth+1), true
		case num == 6 && typ == cursorWireBytes:
			val, n := cursorConsumeBytes(data)
			if n < 0 {
				return json.RawMessage("null"), false
			}
			return cursorProtobufListToJSON(val, depth+1), true
		default:
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				return json.RawMessage("null"), false
			}
			data = data[skip:]
		}
	}
	return json.RawMessage("null"), false
}

func cursorProtobufStructToJSON(data []byte, depth int) json.RawMessage {
	object := make(map[string]json.RawMessage)
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ != cursorWireBytes || num != 1 {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				break
			}
			data = data[skip:]
			continue
		}
		entry, n := cursorConsumeBytes(data)
		if n < 0 {
			break
		}
		data = data[n:]
		key, value := cursorMapEntry(entry)
		if key == "" {
			continue
		}
		object[key] = cursorProtobufValueToJSON(value)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return json.RawMessage("{}")
	}
	return raw
}

func cursorProtobufListToJSON(data []byte, depth int) json.RawMessage {
	var list []json.RawMessage
	for len(data) > 0 {
		num, typ, n := cursorConsumeTag(data)
		if n < 0 {
			break
		}
		data = data[n:]
		if typ != cursorWireBytes || num != 1 {
			skip := cursorSkipField(typ, data)
			if skip < 0 {
				break
			}
			data = data[skip:]
			continue
		}
		val, n := cursorConsumeBytes(data)
		if n < 0 {
			break
		}
		data = data[n:]
		list = append(list, cursorProtobufValueToJSON(val))
	}
	if list == nil {
		list = []json.RawMessage{}
	}
	raw, err := json.Marshal(list)
	if err != nil {
		return json.RawMessage("[]")
	}
	return raw
}

func cursorMCPArgsJSON(args map[string][]byte) string {
	if len(args) == 0 {
		return "{}"
	}
	object := make(map[string]json.RawMessage, len(args))
	for key, value := range args {
		object[key] = cursorProtobufValueToJSON(value)
	}
	raw, err := json.Marshal(object)
	if err != nil {
		return "{}"
	}
	return string(raw)
}

func bytesTrimSpace(raw []byte) []byte {
	return []byte(strings.TrimSpace(string(raw)))
}

func sha256SumBytes(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func flattenCursorMessages(messages []conversationTurn) (history, current string, images []cursorImage, resume []cursorToolResult, err error) {
	if len(messages) == 0 {
		return "", "", nil, nil, fmt.Errorf("messages must not be empty")
	}
	last := messages[len(messages)-1]
	lastContent := parseKiroContent(last, nil)
	if strings.EqualFold(strings.TrimSpace(last.Role), "user") {
		resume = cursorToolResultsFromContent(lastContent)
		current = strings.TrimSpace(lastContent.text)
		images = cursorImagesFromKiro(lastContent.images)
	}
	if len(messages) == 1 && strings.EqualFold(strings.TrimSpace(last.Role), "user") && len(resume) == 0 {
		return "", current, images, nil, nil
	}
	prior := messages
	if strings.EqualFold(strings.TrimSpace(last.Role), "user") {
		prior = messages[:len(messages)-1]
	}
	history, err = flattenCursorHistory(prior)
	if err != nil {
		return "", "", nil, nil, err
	}
	return history, current, images, resume, nil
}

func flattenCursorHistory(messages []conversationTurn) (string, error) {
	var builder strings.Builder
	for _, message := range messages {
		content := parseKiroContent(message, nil)
		switch strings.ToLower(strings.TrimSpace(message.Role)) {
		case "user":
			if text := strings.TrimSpace(content.text); text != "" {
				builder.WriteString("USER:\n")
				builder.WriteString(text)
				builder.WriteByte('\n')
			}
			for _, result := range cursorToolResultsFromContent(content) {
				builder.WriteString("TOOL_RESULT:\n")
				builder.WriteString(result.Text)
				builder.WriteByte('\n')
			}
		case "assistant":
			if text := strings.TrimSpace(content.text); text != "" {
				builder.WriteString("ASSISTANT:\n")
				builder.WriteString(text)
				builder.WriteByte('\n')
			}
			for _, tool := range content.toolUses {
				name, input := cursorToolUseLine(tool)
				builder.WriteString("ASSISTANT_TOOL_CALL:\n")
				builder.WriteString(name)
				builder.WriteByte('(')
				builder.WriteString(input)
				builder.WriteString(")\n")
			}
		}
	}
	return strings.TrimSpace(builder.String()), nil
}

func cursorComposeUserText(history, current string) string {
	history = strings.TrimSpace(history)
	current = strings.TrimSpace(current)
	if history == "" {
		return current
	}
	text := history + "\n\n" + cursorHistoryPreamble
	if current != "" {
		text += "\n\n" + current
	}
	return text
}

func cursorToolResultsFromContent(content kiroContent) []cursorToolResult {
	if len(content.toolResults) == 0 {
		return nil
	}
	results := make([]cursorToolResult, 0, len(content.toolResults))
	for _, raw := range content.toolResults {
		item, _ := raw.(map[string]any)
		if item == nil {
			continue
		}
		id := metadataString(item, "toolUseId")
		if id == "" {
			id = metadataString(item, "call_id")
		}
		if id == "" {
			continue
		}
		text := cursorToolResultText(item["content"])
		isError, _ := item["isError"].(bool)
		if !isError {
			isError = strings.EqualFold(metadataString(item, "status"), "error")
		}
		results = append(results, cursorToolResult{ToolUseID: id, Text: text, IsError: isError})
	}
	return results
}

func cursorToolResultText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		parts := make([]string, 0, len(typed))
		for _, item := range typed {
			block, _ := item.(map[string]any)
			if block == nil {
				continue
			}
			if text := metadataString(block, "text"); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	case nil:
		return ""
	default:
		raw, _ := json.Marshal(typed)
		return string(raw)
	}
}

func cursorToolUseLine(raw any) (name, input string) {
	item, _ := raw.(map[string]any)
	if item == nil {
		return "", "{}"
	}
	name = metadataString(item, "name")
	encoded, err := json.Marshal(item["input"])
	if err != nil || string(encoded) == "null" {
		return name, "{}"
	}
	return name, string(encoded)
}

func cursorImagesFromKiro(images []any) []cursorImage {
	if len(images) == 0 {
		return nil
	}
	out := make([]cursorImage, 0, len(images))
	for _, item := range images {
		block, _ := item.(map[string]any)
		if block == nil {
			continue
		}
		format := strings.TrimSpace(stringValue(block["format"]))
		source, _ := block["source"].(map[string]any)
		data := ""
		if source != nil {
			data = stringValue(source["bytes"])
		}
		raw, err := decodeCursorImageBytes(data)
		if err != nil || len(raw) == 0 || format == "" {
			continue
		}
		if format == "jpg" {
			format = "jpeg"
		}
		out = append(out, cursorImage{MIME: "image/" + format, Data: raw})
	}
	return out
}

// decodeBase64Flexible accepts standard and URL-safe base64, padded or not,
// because upstream image payloads and client image blocks disagree on
// encoding and neither states which one it used.
func decodeBase64Flexible(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, fmt.Errorf("empty base64 payload")
	}
	if raw, err := base64.StdEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawStdEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	if raw, err := base64.URLEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	if raw, err := base64.RawURLEncoding.DecodeString(data); err == nil {
		return raw, nil
	}
	// A data: URL prefix is common in inline image sources.
	if index := strings.Index(data, ","); index >= 0 {
		return decodeBase64Flexible(data[index+1:])
	}
	return nil, fmt.Errorf("invalid base64 payload")
}

// decodeCursorUsableModels reads the GetUsableModels response. The field
// layout is not published, so it accepts the two shapes Cursor has used: a
// repeated field 1 of model-id strings, or a repeated field 1 of messages
// whose field 1 is the id.
func decodeCursorUsableModels(data []byte) []string {
	if len(data) == 0 {
		return nil
	}
	var models []string
	seen := make(map[string]bool, 16)
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			return
		}
		seen[id] = true
		models = append(models, id)
	}
	raw := cursorStripConnect(data)
	for len(raw) > 0 {
		num, typ, n := cursorConsumeTag(raw)
		if n < 0 {
			break
		}
		raw = raw[n:]
		if typ != cursorWireBytes || num != 1 {
			skip := cursorSkipField(typ, raw)
			if skip < 0 {
				break
			}
			raw = raw[skip:]
			continue
		}
		val, n := cursorConsumeBytes(raw)
		if n < 0 {
			break
		}
		raw = raw[n:]
		if id := string(val); isCursorModelID(id) {
			add(id)
			continue
		}
		if id := cursorInnerString(val, 1); isCursorModelID(id) {
			add(id)
			continue
		}
		if id := cursorInnerString(val, 2); isCursorModelID(id) {
			add(id)
		}
	}
	return models
}

// isCursorModelID rejects protobuf noise (empty, binary) so a mis-shaped
// response degrades to the fallback catalog instead of advertising garbage.
func isCursorModelID(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return strings.ContainsAny(value, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")
}

func decodeCursorImageBytes(data string) ([]byte, error) {
	data = strings.TrimSpace(data)
	if data == "" {
		return nil, fmt.Errorf("empty image")
	}
	return decodeBase64Flexible(data)
}
