/*
	This is a database interface library using Exasol's websocket API
    https://github.com/exasol/websocket-api/blob/master/WebsocketAPI.md

	TODOs:
	1) Support connection compression
	2) Support connection encryption
	3) Convert to database/sql interface
	4) Implement timeouts for all query types


	AUTHOR

	Grant Street Group <developers@grantstreet.com>

	COPYRIGHT AND LICENSE

	This software is Copyright (c) 2019 by Grant Street Group.
	This is free software, licensed under:
	    MIT License
*/

package exasol

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
	"os/user"
	"regexp"
	"runtime"
	"strconv"
	"sync"

	"github.com/gorilla/websocket"
)

/*--- Public Interface ---*/

type ConnConf struct {
	Host          string
	Port          uint16
	Username      string
	Password      string
	ClientName    string
	Timeout       uint32 // In Seconds
	SuppressError bool   // Server errors are logged to Error by default
	// TODO try compressionEnabled: true
	Logger         Logger // Optional for better control over logging
	CachePrepStmts bool
}

type Conn struct {
	Conf      ConnConf
	SessionID uint64
	Stats     map[string]int

	log           Logger
	ws            *websocket.Conn
	prepStmtCache map[string]*prepStmt
	mux           sync.Mutex
}

type DataType struct {
	Type    string `json:"type"`
	Size    int    `json:"size"`
	Prec    int    `json:"precision"`
	Scale   int    `json:"scale"`
	CharSet string `json:"characterSet,omitempty"`
}

func Connect(conf ConnConf) (*Conn, error) {

	c := &Conn{
		Conf:          conf,
		Stats:         map[string]int{},
		log:           conf.Logger,
		prepStmtCache: map[string]*prepStmt{},
	}

	if c.log == nil {
		c.log = newDefaultLogger()
	}

	err := c.wsConnect()
	if err != nil {
		return nil, err
	}

	err = c.login()
	if err != nil {
		return nil, err
	}

	if conf.Timeout > 0 {
		c.SetTimeout(conf.Timeout)
	}
	return c, nil
}

func (c *Conn) Disconnect() {
	c.log.Info("Disconnecting SessionID:", c.SessionID)

	for _, ps := range c.prepStmtCache {
		c.closePrepStmt(ps.sth)
	}
	_, err := c.send(&disconnectJSON{Command: "disconnect"})
	if err != nil {
		c.log.Warning("Unable to disconnect from Exasol: ", err)
	}
	c.ws.Close()
	c.ws = nil
}

func (c *Conn) GetSessionAttr() (map[string]interface{}, error) {
	req := &sendAttrJSON{Command: "getAttributes"}
	res, err := c.send(req)
	return res, err
}

func (c *Conn) EnableAutoCommit() {
	c.log.Info("Enabling AutoCommit")
	c.send(&sendAttrJSON{
		Command:    "setAttributes",
		Attributes: attrJSON{AutoCommit: true},
	})
}

func (c *Conn) DisableAutoCommit() {
	c.log.Info("Disabling AutoCommit")
	// We have to roll our own map because attrJSON
	// needs to have AutoCommit set to omitempty which
	// causes autocommit=false not to be sent :-(
	c.send(map[string]interface{}{
		"command": "setAttributes",
		"attributes": map[string]interface{}{
			"autocommit": false,
		},
	})
}

func (c *Conn) Rollback() error {
	c.log.Info("Rolling back transaction")
	_, err := c.Execute("ROLLBACK")
	return err
}

func (c *Conn) Commit() error {
	c.log.Info("Committing transaction")
	_, err := c.Execute("COMMIT")
	return err
}

// TODO change optional args into an ExecConf struct
// Optional args are binds, default schema, colDefs, isColumnar flag
func (c *Conn) Execute(sql string, args ...interface{}) (map[string]interface{}, error) {
	var binds [][]interface{}
	if len(args) > 0 && args[0] != nil {
		// TODO make the binds optionally just []interface{}
		binds = args[0].([][]interface{})
	}
	var schema string
	if len(args) > 1 && args[1] != nil {
		schema = args[1].(string)
	}
	var dataTypes []interface{}
	if len(args) > 2 && args[2] != nil {
		dataTypes = args[2].([]interface{})
	}
	isColumnar := false // Whether or not the passed-in binds are columnar
	if len(args) > 3 && args[3] != nil {
		isColumnar = args[3].(bool)
	}

	// Just a simple execute (no prepare) if there are no binds
	if binds == nil || len(binds) == 0 ||
		binds[0] == nil || len(binds[0]) == 0 {
		c.log.Debug("Execute: ", sql)
		req := &executeStmtJSON{
			Command:    "execute",
			Attributes: attrJSON{CurrentSchema: schema},
			SQLtext:    sql,
		}
		return c.send(req)
	}

	// Else need to send data so do a prepare + execute
	ps, err := c.getPrepStmt(schema, sql)
	if err != nil {
		return nil, err
	}

	if dataTypes != nil {
		for i := range dataTypes {
			ps.columnDefs[i].(map[string]interface{})["dataType"] = dataTypes[i]
		}
	}

	if !isColumnar {
		binds = Transpose(binds)
	}
	numCols := len(binds)
	numRows := len(binds[0])

	c.log.Debugf("Executing %d x %d stmt", numCols, numRows)
	execReq := &execPrepStmtJSON{
		Command:         "executePreparedStatement",
		StatementHandle: int(ps.sth),
		NumColumns:      numCols,
		NumRows:         numRows,
		Columns:         ps.columnDefs,
		Data:            binds,
	}

	res, err := c.send(execReq)

	if err != nil &&
		regexp.MustCompile("Statement handle not found").MatchString(err.Error()) {
		// Not sure what causes this but I've seen it happen. So just try again.
		c.log.Warning("Statement handle not found:", ps.sth)
		delete(c.prepStmtCache, sql)
		ps, err := c.getPrepStmt(schema, sql)
		if err != nil {
			return nil, err
		}
		c.log.Warning("Retrying with:", ps.sth)
		execReq.StatementHandle = int(ps.sth)
		res, err = c.send(execReq)
	}
	if !c.Conf.CachePrepStmts {
		c.closePrepStmt(ps.sth)
	}

	return res, err
}

func (c *Conn) FetchChan(sql string, args ...interface{}) (<-chan []interface{}, error) {
	var binds []interface{}
	if len(args) > 0 && args[0] != nil {
		binds = args[0].([]interface{})
	}
	var schema string
	if len(args) > 1 && args[1] != nil {
		schema = args[1].(string)
	}

	response, err := c.Execute(sql, [][]interface{}{binds}, schema)
	if err != nil {
		return nil, err
	}
	if response["numResults"].(float64) != 1 {
		return nil, fmt.Errorf("Unexpected numResults: %v", response["numResults"].(float64))
	}
	results := response["results"].([]interface{})[0].(map[string]interface{})
	if results["resultSet"] == nil {
		return nil, fmt.Errorf("Missing websocket API resultset")
	}
	rs := results["resultSet"].(map[string]interface{})

	ch := make(chan []interface{}, 1000)

	go func() {
		if rs["numRows"].(float64) == 0 {
			// Do nothing
		} else if rsh, ok := rs["resultSetHandle"].(float64); ok {
			for i := float64(0); i < rs["numRows"].(float64); {
				fetchReq := &fetchJSON{
					Command:         "fetch",
					ResultSetHandle: rsh,
					StartPosition:   i,
					NumBytes:        64 * 1024 * 1024, // Max allowed
				}
				chunk, err := c.send(fetchReq)
				if err != nil {
					panic(err)
				}
				i += chunk["numRows"].(float64)
				transposeToChan(ch, chunk["data"].([]interface{}))
			}

			closeRSReq := &closeResultSetJSON{
				Command:          "closeResultSet",
				ResultSetHandles: []float64{rsh},
			}
			_, err = c.send(closeRSReq)
			if err != nil {
				c.log.Warning("Unable to close result set:", err)
			}
		} else {
			transposeToChan(ch, rs["data"].([]interface{}))
		}
		close(ch)
	}()

	return ch, nil
}

// For large datasets use FetchChan to avoid buffering all the data in memory
func (c *Conn) FetchSlice(sql string, args ...interface{}) (res [][]interface{}, err error) {
	resChan, err := c.FetchChan(sql, args...)
	if err != nil {
		return
	}
	for row := range resChan {
		res = append(res, row)
	}
	return
}

// Gets a sync.Mutext lock on the handle.
// Allows coordinating use of the handle across multiple Go routines
func (c *Conn) Lock()   { c.mux.Lock() }
func (c *Conn) Unlock() { c.mux.Unlock() }

/*--- Private Routines ---*/

type attrJSON struct {
	AutoCommit    bool   `json:"autocommit,omitempty"`
	CurrentSchema string `json:"currentSchema,omitempty"`
	QueryTimeout  uint32 `json:"queryTimeout,omitempty"`
}

type loginJSON struct {
	Command         string `json:"command"`
	ProtocolVersion uint16 `json:"protocolVersion"`
}

type authJSON struct {
	Username         string `json:"username"`
	Password         string `json:"password"`
	UseCompression   bool   `json:"useCompression"`
	ClientName       string `json:"clientName,omitempty"`
	DriverName       string `json:"driverName,omitempty"`
	ClientOsUsername string `json:"clientOsUsername,omitempty"`
	ClientOs         string `json:"clientOs,omitempty"`
	// TODO specify these
	//SessionId        uint64 `json:"useCompression,omitempty"`
	//ClientLanguage   string `json:"useCompression,omitempty"`
	//ClientVersion    string `json:"useCompression,omitempty"`
	//ClientRuntime    string `json:"useCompression,omitempty"`
	Attributes attrJSON `json:"attributes,omitempty"`
}

type sendAttrJSON struct {
	Command    string   `json:"command"`
	Attributes attrJSON `json:"attributes"`
}

type disconnectJSON struct {
	Command    string   `json:"command"`
	Attributes attrJSON `json:"attributes,omitempty"`
}

type executeStmtJSON struct {
	Command    string   `json:"command"`
	Attributes attrJSON `json:"attributes,omitempty"`
	SQLtext    string   `json:"sqlText"`
}

type fetchJSON struct {
	Command    string   `json:"command"`
	Attributes attrJSON `json:"attributes,omitempty"`
	// TODO change these back to ints? fetch change select * from exacols gave strange websocket error
	ResultSetHandle float64 `json:"resultSetHandle"`
	StartPosition   float64 `json:"startPosition"`
	NumBytes        int     `json:"numBytes"`
}

type closeResultSetJSON struct {
	Command          string    `json:"command"`
	Attributes       attrJSON  `json:"attributes,omitempty"`
	ResultSetHandles []float64 `json:"resultSetHandles"`
}

func (c *Conn) login() error {
	loginReq := &loginJSON{
		Command:         "login",
		ProtocolVersion: 1,
	}
	res, err := c.send(loginReq) // TODO change req to pointer
	if err != nil {
		return err
	}

	pubKeyMod, _ := hex.DecodeString(res["publicKeyModulus"].(string))
	var modulus big.Int
	modulus.SetBytes(pubKeyMod)

	pubKeyExp, _ := strconv.ParseUint(res["publicKeyExponent"].(string), 16, 32)

	pubKey := rsa.PublicKey{
		N: &modulus,
		E: int(pubKeyExp),
	}
	password := []byte(c.Conf.Password)
	encPass, err := rsa.EncryptPKCS1v15(rand.Reader, &pubKey, password)
	if err != nil {
		return fmt.Errorf("Password encryption error: %s", err)
	}
	b64Pass := base64.StdEncoding.EncodeToString(encPass)

	osUser, _ := user.Current()

	authReq := &authJSON{
		Username:         c.Conf.Username,
		Password:         b64Pass,
		UseCompression:   false, // TODO: See if we can get compression working
		ClientName:       c.Conf.ClientName,
		DriverName:       "go-exasol",
		ClientOs:         runtime.GOOS,
		ClientOsUsername: osUser.Username,
		Attributes:       attrJSON{AutoCommit: true}, // Default AutoCommit to on
	}
	_, err = c.send(authReq)
	if err != nil {
		return fmt.Errorf("Unable authenticate with Exasol: %s", err)
	}

	// Unfortunately the sessionID that is returned by the
	// login request is sent as a 20 digit number which Go
	// unmarshals into a float64 which when converted into
	// an integer no longer exactly matches the original number.
	// So we have to ask for the session separately
	// TODO: We need a solution for this to avoid the extra query
	resp, err := c.Execute("SELECT CURRENT_SESSION")
	if err != nil {
		return fmt.Errorf("Unable fetch session from Exasol: %s", err)
	}
	session, err := strconv.ParseUint(
		resp["results"].([]interface{})[0].(map[string]interface{})["resultSet"].(map[string]interface{})["data"].([]interface{})[0].([]interface{})[0].(string),
		10, 64,
	)
	if err != nil {
		return fmt.Errorf("Unable parse session from Exasol: %s", err)
	}
	c.SessionID = session
	c.log.Info("Connected SessionID:", c.SessionID)
	c.ws.EnableWriteCompression(false)

	return nil
}

func (c *Conn) SetTimeout(timeout uint32) {
	c.send(&sendAttrJSON{
		Command:    "setAttributes",
		Attributes: attrJSON{QueryTimeout: timeout},
	})
}
