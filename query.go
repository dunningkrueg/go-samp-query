package sampquery

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"net"
	"strings"
	"time"

	"github.com/pkg/errors"
	"github.com/saintfish/chardet"
	"golang.org/x/text/encoding/htmlindex"
)

// Server contains all the information retreived from the server query API.
type Server struct {
	Address    string            `json:"address"`
	Hostname   string            `json:"hostname"`
	Players    int               `json:"players"`
	MaxPlayers int               `json:"max_players"`
	Gamemode   string            `json:"gamemode"`
	Language   string            `json:"language"`
	Password   bool              `json:"password"`
	Rules      map[string]string `json:"rules"`
	Ping       int               `json:"ping"`
	IsOmp      bool              `json:"isOmp"`
}

// QueryType represents a query method from the SA:MP set: i, r, c, d, x, p
type QueryType uint8

const (
	// Info is the 'i' packet type
	Info QueryType = 'i'
	// Rules is the 'r' packet type
	Rules QueryType = 'r'
	// Players is the 'c' packet type
	Players QueryType = 'c'
	// Ping is the 'p' packet type
	Ping QueryType = 'p'
	// IsOmp is the 'o' packet type
	IsOmp QueryType = 'o'
)

// Query stores state for masterlist queries
type Query struct {
	addr *net.UDPAddr
	Data Server
}

// GetServerInfo wraps a set of queries and returns a new Server object with the available fields
// populated. `attemptDecode` determines whether or not to attempt to decode ANSI into Unicode from
// servers that use different codepages such as Cyrillic. This function can panic if the socket it
// opens fails to close for whatever reason.
func GetServerInfo(ctx context.Context, host string, attemptDecode bool) (server Server, err error) {
	query, err := NewQuery(host)
	if err != nil {
		return
	}
	defer func() {
		if e := query.Close(); e != nil {
			panic(e)
		}
	}()

	server, err = query.GetInfo(ctx, attemptDecode)
	if err != nil {
		return
	}
	server.Address = host

	server.Rules, err = query.GetRules(ctx)
	if err != nil {
		return
	}

	ping, err := query.GetPing(ctx)
	if err != nil {
		return
	}
	server.Ping = int(ping)

	var isOmp bool
	requiresAdditionalOmpCheck := false
	ver, found := server.Rules["version"]
	_, found2 := server.Rules["allow_DL"]

	if found {
		requiresAdditionalOmpCheck = strings.Contains(ver, "omp ")
	} else {
		if found2 {
			requiresAdditionalOmpCheck = true
		}
	}

	if !requiresAdditionalOmpCheck {
		isOmp = query.GetOmpValidity(ctx)
	} else {
		isOmp = true
	}

	server.IsOmp = isOmp
	return
}

// NewQuery creates a new query handler for a server
func NewQuery(host string) (query *Query, err error) {
	query = new(Query)

	query.addr, err = net.ResolveUDPAddr("udp", host)
	if err != nil {
		return nil, errors.Wrap(err, "failed to resolve host")
	}

	return query, nil
}

// Close closes a query manager's connection
func (query *Query) Close() error {
	return nil
}

// SendQuery writes a SA:MP format query with the specified opcode, returns the raw response bytes
func (query *Query) SendQuery(ctx context.Context, opcode QueryType) (response []byte, err error) {
	request := new(bytes.Buffer)

	port := [2]byte{
		byte(query.addr.Port & 0xFF),
		byte((query.addr.Port >> 8) & 0xFF),
	}

	if err = binary.Write(request, binary.LittleEndian, []byte("SAMP")); err != nil {
		return
	}
	if err = binary.Write(request, binary.LittleEndian, query.addr.IP.To4()); err != nil {
		return
	}
	if err = binary.Write(request, binary.LittleEndian, port[0]); err != nil {
		return
	}
	if err = binary.Write(request, binary.LittleEndian, port[1]); err != nil {
		return
	}
	if err = binary.Write(request, binary.LittleEndian, opcode); err != nil {
		return
	}

	if opcode == Ping || opcode == IsOmp {
		p := make([]byte, 4)
		_, err = rand.Read(p)
		if err != nil {
			return
		}
		if err = binary.Write(request, binary.LittleEndian, p); err != nil {
			return
		}
	}

	conn, err := openConnection(query.addr)
	if err != nil {
		return
	}
	defer conn.Close()

	_, err = conn.Write(request.Bytes())
	if err != nil {
		return nil, errors.Wrap(err, "failed to write")
	}

	type resultData struct {
		data  []byte
		bytes int
		err   error
	}
	waitResult := make(chan resultData, 1)

	go func() {
		response := make([]byte, 2048)

		if opcode == IsOmp {
			conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		}

		n, errInner := conn.Read(response)
		if errInner != nil {
			waitResult <- resultData{err: errors.Wrap(errInner, "failed to read response")}
			return
		}
		if n > cap(response) {
			waitResult <- resultData{err: errors.New("read response over buffer capacity")}
			return
		}
		waitResult <- resultData{data: response, bytes: n}
	}()

	var result resultData
	select {
	case <-ctx.Done():
		{
			if opcode == IsOmp {
				return nil, nil
			}
			return nil, errors.New("socket read timed out")
		}

	case result = <-waitResult:
		break
	}

	if result.err != nil {
		return nil, result.err
	}

	if result.bytes < 11 {
		return nil, errors.New("response is less than 11 bytes")
	}

	return result.data[:result.bytes], nil
}

// GetPing sends and receives a packet to measure ping
func (query *Query) GetPing(ctx context.Context) (ping time.Duration, err error) {
	t := time.Now()
	_, err = query.SendQuery(ctx, Ping)
	if err != nil {
		return 0, err
	}
	ping = time.Now().Sub(t)

	return
}

// GetOmpValidity sends and receives a packet to check if server is using open.mp or not
func (query *Query) GetOmpValidity(ctx context.Context) bool {
	var res, _ = query.SendQuery(ctx, IsOmp)
	if res == nil {
		return false
	}

	return true
}

// GetInfo returns the core server info for displaying on the browser list.
func (query *Query) GetInfo(ctx context.Context, attemptDecode bool) (server Server, err error) {
	response, err := query.SendQuery(ctx, Info)
	if err != nil {
		return server, err
	}

	ptr := 11

	server.Password = (response[ptr] == 1)
	ptr++

	server.Players = int(binary.LittleEndian.Uint16(response[ptr : ptr+2]))
	ptr += 2

	server.MaxPlayers = int(binary.LittleEndian.Uint16(response[ptr : ptr+2]))
	ptr += 2

	hostnameLen := int(binary.LittleEndian.Uint16(response[ptr : ptr+4]))
	ptr += 4

	hostnameRaw := response[ptr : ptr+hostnameLen]
	ptr += hostnameLen

	gamemodeLen := int(binary.LittleEndian.Uint16(response[ptr : ptr+4]))
	ptr += 4

	gamemodeRaw := response[ptr : ptr+gamemodeLen]
	ptr += gamemodeLen

	languageLen := int(binary.LittleEndian.Uint16(response[ptr : ptr+4]))
	ptr += 4

	languageRaw := response[ptr : ptr+languageLen]

	guessHelper := bytes.Join([][]byte{
		hostnameRaw,
		gamemodeRaw,
		languageRaw,
	}, []byte(" "))

	if attemptDecode {
		languageStr := ""
		if languageLen > 0 {
			languageStr = string(languageRaw)
		}
		server.Gamemode = attemptDecodeANSI(gamemodeRaw, guessHelper, languageStr)
		server.Hostname = attemptDecodeANSI(hostnameRaw, guessHelper, languageStr)
	} else {
		server.Gamemode = string(gamemodeRaw)
		server.Hostname = string(hostnameRaw)
	}

	if languageLen > 0 && attemptDecode {
		server.Language = attemptDecodeANSI(languageRaw, guessHelper, string(languageRaw))
	} else {
		server.Language = "-"
	}
	return
}

// GetRules returns a map of rule properties from a server. The query uses established keys
// such as "Map" and "Version"
func (query *Query) GetRules(ctx context.Context) (rules map[string]string, err error) {
	response, err := query.SendQuery(ctx, Rules)
	responseLen := len(response)
	if err != nil {
		return rules, err
	}

	rules = make(map[string]string)

	if responseLen < 20 {
		return rules, nil
	}

	var (
		key    string
		val    string
		keyLen int
		valLen int
	)

	// fmt.Println("starting GetRules for", query.addr)
	ptr := 11
	amount := binary.LittleEndian.Uint16(response[ptr : ptr+2])
	ptr += 2

	for i := uint16(0); i < amount && ptr < responseLen; i++ {
		if ptr >= responseLen {
			break
		}

		keyLen = int(response[ptr])
		ptr++

		if ptr+keyLen > responseLen {
			break
		}

		key = string(response[ptr : ptr+keyLen])
		ptr += keyLen

		if ptr >= responseLen {
			break
		}

		valLen = int(response[ptr])
		ptr++

		if ptr+valLen > responseLen {
			break
		}

		val = string(response[ptr : ptr+valLen])
		ptr += valLen

		rules[key] = val
	}

	return
}

// GetPlayers simply returns a slice of strings, score is rather arbitrary so it's omitted.
func (query *Query) GetPlayers(ctx context.Context) (players []string, err error) {
	response, err := query.SendQuery(ctx, Players)
	if err != nil {
		return
	}

	var (
		count  uint16
		length int
	)

	ptr := 11
	count = binary.LittleEndian.Uint16(response[ptr : ptr+2])
	ptr += 2

	players = make([]string, count)

	for i := uint16(0); i < count; i++ {
		length = int(response[ptr])
		ptr++

		players[i] = string(response[ptr : ptr+length])
		ptr += length
		ptr += 4 // score, unused
	}

	return players, nil
}

func openConnection(addr *net.UDPAddr) (conn *net.UDPConn, err error) {
	conn, err = net.DialUDP("udp", nil, addr)
	if err != nil {
		return nil, errors.Wrap(err, "failed to dial")
	}
	return
}

func attemptDecodeANSI(input []byte, extra []byte, language string) (result string) {
	// Fast path: If language is known, use the appropriate encoding
	if encoding := getEncodingForLanguage(language); encoding != "" {
		e, err := htmlindex.Get(encoding)
		if err == nil {
			dec := e.NewDecoder()
			decoded, err := dec.Bytes(input)
			if err == nil {
				return string(decoded)
			}
		}
	}

	result = string(input)
	detector, err := chardet.NewTextDetector().DetectBest(extra)
	if err != nil {
		return
	}
	e, err := htmlindex.Get(detector.Charset)
	if err != nil {
		return
	}
	dec := e.NewDecoder()
	decoded, err := dec.Bytes(input)
	if err != nil {
		return
	}
	return string(decoded)
}

// getEncodingForLanguage returns the appropriate encoding based on server language
func getEncodingForLanguage(language string) string {
	language = strings.ToLower(language)

	// For Cyrillic languages
	if language == "russian" || language == "ru" || language == "ru-ru" ||
		language == "ukrainian" || language == "uk" || language == "uk-ua" ||
		language == "belarusian" || language == "be" ||
		language == "bulgarian" || language == "bg" ||
		language == "macedonian" || language == "mk" ||
		language == "serbian" || language == "sr" {
		return "windows-1251"
	}

	if language == "chinese" || language == "zh" || language == "zh-cn" || language == "zh-tw" {
		return "gb18030"
	}

	if language == "korean" || language == "ko" || language == "ko-kr" {
		return "euc-kr"
	}

	if language == "japanese" || language == "ja" || language == "ja-jp" {
		return "shift-jis"
	}

	if language == "polish" || language == "pl" ||
		language == "czech" || language == "cs" ||
		language == "slovak" || language == "sk" ||
		language == "hungarian" || language == "hu" ||
		language == "romanian" || language == "ro" {
		return "windows-1250"
	}

	if language == "turkish" || language == "tr" ||
		language == "kurdish" || language == "ku" {
		return "windows-1254"
	}

	if language == "greek" || language == "el" {
		return "windows-1253"
	}

	if language == "arabic" || language == "ar" ||
		language == "persian" || language == "fa" ||
		language == "urdu" || language == "ur" {
		return "windows-1256"
	}

	// If not recognized, return empty string
	return ""
}
