/*
 * Copyright (c) "Neo4j"
 * Neo4j Sweden AB [https://neo4j.com]
 *
 * This file is part of Neo4j.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 *  Unless required by applicable law or agreed to in writing, software
 *  distributed under the License is distributed on an "AS IS" BASIS,
 *  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 *  See the License for the specific language governing permissions and
 *  limitations under the License.
 */

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/db"
)

// Handles a testkit backend session.
// Tracks all objects (and errors) that is created by testkit frontend.
type backend struct {
	rd                   *bufio.Reader // Socket to read requests from
	wr                   io.Writer     // Socket to write responses (and logs) on, don't buffer (WriteString on bufio was weird...)
	drivers              map[string]neo4j.DriverWithContext
	sessionStates        map[string]*sessionState
	results              map[string]neo4j.ResultWithContext
	managedTransactions  map[string]neo4j.ManagedTransaction
	explicitTransactions map[string]neo4j.ExplicitTransaction
	recordedErrors       map[string]error
	resolvedAddresses    map[string][]interface{}
	id                   int // ID to use for next object created by frontend
	wrLock               sync.Mutex
}

// To implement transactional functions a bit of extra state is needed on the
// driver session.
type sessionState struct {
	session          neo4j.SessionWithContext
	retryableState   int
	retryableErrorId string
}

const (
	retryableNothing  = 0
	retryablePositive = 1
	retryableNegative = -1
)

var ctx = context.Background()

func newBackend(rd *bufio.Reader, wr io.Writer) *backend {
	return &backend{
		rd:                   rd,
		wr:                   wr,
		drivers:              make(map[string]neo4j.DriverWithContext),
		sessionStates:        make(map[string]*sessionState),
		results:              make(map[string]neo4j.ResultWithContext),
		managedTransactions:  make(map[string]neo4j.ManagedTransaction),
		explicitTransactions: make(map[string]neo4j.ExplicitTransaction),
		recordedErrors:       make(map[string]error),
		resolvedAddresses:    make(map[string][]interface{}),
		id:                   0,
	}
}

type frontendError struct {
	msg string
}

func (e *frontendError) Error() string {
	return e.msg
}

func (b *backend) writeLine(s string) error {
	bs := []byte(s + "\n")
	_, err := b.wr.Write(bs)
	return err
}

func (b *backend) writeLineLocked(s string) error {
	b.wrLock.Lock()
	defer b.wrLock.Unlock()
	return b.writeLine(s)
}

// Reads and writes to the socket until it is closed
func (b *backend) serve() {
	for b.process() {
	}
}

func (b *backend) setError(err error) string {
	id := b.nextId()
	b.recordedErrors[id] = err
	return id
}

func (b *backend) writeError(err error) {
	// Convert error if it is a known type of error.
	// This is very simple right now, no extra information is sent at all just keep
	// track of this error so that we can reuse the real thing within a retryable tx
	fmt.Printf("Error: %s (%T)\n", err.Error(), err)
	code := ""
	_, isHydrationError := err.(*db.ProtocolError)
	tokenErr, isTokenExpiredErr := err.(*neo4j.TokenExpiredError)
	if isTokenExpiredErr {
		code = tokenErr.Code
	}
	if neo4j.IsNeo4jError(err) {
		code = err.(*db.Neo4jError).Code
	}
	isDriverError := isHydrationError ||
		isTokenExpiredErr ||
		neo4j.IsNeo4jError(err) ||
		neo4j.IsUsageError(err) ||
		neo4j.IsConnectivityError(err) ||
		neo4j.IsTransactionExecutionLimit(err) ||
		err.Error() == "Invalid transaction handle"

	if isDriverError {
		id := b.setError(err)
		b.writeResponse("DriverError", map[string]interface{}{
			"id":        id,
			"errorType": strings.Split(err.Error(), ":")[0],
			"msg":       err.Error(),
			"code":      code})
		return
	}

	// This is an error that originated in frontend
	frontendErr, isFrontendErr := err.(*frontendError)
	if isFrontendErr {
		b.writeResponse("FrontendError", map[string]interface{}{"msg": frontendErr.msg})
		return
	}

	// TODO: Return the other kinds of errors as well...

	// Unknown error, interpret this as a backend error
	// Report this to frontend and close the connection
	// This simplifies debugging errors from the frontend perspective, it will also make sure
	// that the frontend doesn't hang when backend suddenly disappears.
	b.writeResponse("BackendError", map[string]interface{}{"msg": err.Error()})
}

func (b *backend) nextId() string {
	b.id++
	return fmt.Sprintf("%d", b.id)
}

func (b *backend) process() bool {
	request := ""
	inRequest := false

	for {
		line, err := b.rd.ReadString('\n')
		if err != nil {
			return false
		}

		switch line {
		case "#request begin\n":
			if inRequest {
				panic("Already in request")
			}
			inRequest = true
		case "#request end\n":
			if !inRequest {
				panic("End while not in request")
			}
			b.handleRequest(b.toRequest(request))
			request = ""
			inRequest = false
			return true
		default:
			if !inRequest {
				panic("Line while not in request")
			}

			request = request + line
		}
	}
}

func (b *backend) writeResponse(name string, data interface{}) {
	response := map[string]interface{}{"name": name, "data": data}
	responseJson, err := json.Marshal(response)
	fmt.Printf("RES: %s\n", name) //string(responseJson))
	if err != nil {
		panic(err.Error())
	}
	// Make sure that logging framework doesn't write anything inbetween here...
	b.wrLock.Lock()
	defer b.wrLock.Unlock()
	err = b.writeLine("#response begin")
	if err != nil {
		panic(err.Error())
	}
	err = b.writeLine(string(responseJson))
	if err != nil {
		panic(err.Error())
	}
	err = b.writeLine("#response end")
	if err != nil {
		panic(err.Error())
	}
}

func (b *backend) toRequest(s string) map[string]interface{} {
	req := map[string]interface{}{}
	err := json.Unmarshal([]byte(s), &req)
	if err != nil {
		panic(fmt.Sprintf("Unable to parse: '%s' as a request: %s", s, err))
	}
	return req
}

func (b *backend) toTransactionConfigApply(data map[string]interface{}) func(*neo4j.TransactionConfig) {
	txConfig := neo4j.TransactionConfig{Timeout: math.MinInt}
	// Optional transaction meta data
	if data["txMeta"] != nil {
		txConfig.Metadata = data["txMeta"].(map[string]interface{})
	}
	// Optional timeout in milliseconds
	if data["timeout"] != nil {
		txConfig.Timeout = time.Millisecond * time.Duration(data["timeout"].(float64))
	}
	return func(conf *neo4j.TransactionConfig) {
		if txConfig.Metadata != nil {
			conf.Metadata = txConfig.Metadata
		}
		if txConfig.Timeout != math.MinInt {
			conf.Timeout = txConfig.Timeout
		}
	}
}

func (b *backend) toCypherAndParams(data map[string]interface{}) (string, map[string]interface{}) {
	cypher := data["cypher"].(string)
	params, _ := data["params"].(map[string]interface{})
	for i, p := range params {
		params[i] = cypherToNative(p)
	}
	return cypher, params
}

func (b *backend) handleTransactionFunc(isRead bool, data map[string]interface{}) {
	sid := data["sessionId"].(string)
	sessionState := b.sessionStates[sid]
	blockingRetry := func(tx neo4j.ManagedTransaction) (interface{}, error) {
		sessionState.retryableState = retryableNothing
		// Instruct client to start doing its work
		txId := b.nextId()
		b.managedTransactions[txId] = tx
		b.writeResponse("RetryableTry", map[string]interface{}{"id": txId})
		// Process all things that the client might do within the transaction
		for {
			b.process()
			switch sessionState.retryableState {
			case retryablePositive:
				// Client succeeded and wants to commit
				return nil, nil
			case retryableNegative:
				// Client failed in some way
				if sessionState.retryableErrorId != "" {
					return nil, b.recordedErrors[sessionState.retryableErrorId]
				} else {
					return nil, &frontendError{msg: "Error from client"}
				}
			case retryableNothing:
				// Client did something not related to the retryable state
			}
		}
	}
	var err error
	if isRead {
		_, err = sessionState.session.ExecuteRead(ctx, blockingRetry, b.toTransactionConfigApply(data))
	} else {
		_, err = sessionState.session.ExecuteWrite(ctx, blockingRetry, b.toTransactionConfigApply(data))
	}

	if err != nil {
		b.writeError(err)
	} else {
		b.writeResponse("RetryableDone", map[string]interface{}{})
	}
}

func (b *backend) customAddressResolverFunction() neo4j.ServerAddressResolver {
	return func(address neo4j.ServerAddress) []neo4j.ServerAddress {
		id := b.nextId()
		b.writeResponse("ResolverResolutionRequired", map[string]string{
			"id":      id,
			"address": fmt.Sprintf("%s:%s", address.Hostname(), address.Port()),
		})
		for {
			b.process()
			if addresses, ok := b.resolvedAddresses[id]; ok {
				result := make([]neo4j.ServerAddress, len(addresses))
				for i, address := range addresses {
					result[i] = NewServerAddress(address.(string))
				}
				return result
			}
		}
	}

}

type serverAddress struct {
	hostname string
	port     string
}

func NewServerAddress(address string) neo4j.ServerAddress {
	parsedAddress, err := url.Parse("//" + address)
	if err != nil {
		panic(err)
	}
	return serverAddress{
		hostname: parsedAddress.Hostname(),
		port:     parsedAddress.Port(),
	}
}

func (s serverAddress) Hostname() string {
	return s.hostname
}

func (s serverAddress) Port() string {
	return s.port
}

func (b *backend) handleRequest(req map[string]interface{}) {
	name := req["name"].(string)
	data := req["data"].(map[string]interface{})

	fmt.Printf("REQ: %s\n", name)
	switch name {

	case "ResolverResolutionCompleted":
		requestId := data["requestId"].(string)
		addresses := data["addresses"].([]interface{})
		b.resolvedAddresses[requestId] = addresses

	case "NewDriver":
		// Parse authorization token
		var authToken neo4j.AuthToken
		authTokenMap := data["authorizationToken"].(map[string]interface{})["data"].(map[string]interface{})
		switch authTokenMap["scheme"] {
		case "basic":
			realm, ok := authTokenMap["realm"].(string)
			if !ok {
				realm = ""
			}
			authToken = neo4j.BasicAuth(
				authTokenMap["principal"].(string),
				authTokenMap["credentials"].(string),
				realm)
		case "kerberos":
			authToken = neo4j.KerberosAuth(authTokenMap["credentials"].(string))
		case "bearer":
			authToken = neo4j.BearerAuth(authTokenMap["credentials"].(string))
		default:
			authToken = neo4j.CustomAuth(
				authTokenMap["scheme"].(string),
				authTokenMap["principal"].(string),
				authTokenMap["credentials"].(string),
				authTokenMap["realm"].(string),
				authTokenMap["parameters"].(map[string]interface{}))
		}
		// Parse URI (or rather type cast)
		uri := data["uri"].(string)
		driver, err := neo4j.NewDriverWithContext(uri, authToken, func(c *neo4j.Config) {
			// Setup custom logger that redirects log entries back to frontend
			c.Log = &streamLog{writeLine: b.writeLineLocked}
			// Optional custom user agent from frontend
			userAgentX := data["userAgent"]
			if userAgentX != nil {
				c.UserAgent = userAgentX.(string)
			}
			if data["resolverRegistered"].(bool) {
				c.AddressResolver = b.customAddressResolverFunction()
			}
			if data["connectionAcquisitionTimeoutMs"] != nil {
				c.ConnectionAcquisitionTimeout = time.Millisecond * time.Duration(data["connectionAcquisitionTimeoutMs"].(float64))
			}
			if data["maxConnectionPoolSize"] != nil {
				c.MaxConnectionPoolSize = int(data["maxConnectionPoolSize"].(float64))
			}
			if data["fetchSize"] != nil {
				c.FetchSize = int(data["fetchSize"].(float64))
			}
			if data["maxTxRetryTimeMs"] != nil {
				c.MaxTransactionRetryTime = time.Millisecond * time.Duration(data["maxTxRetryTimeMs"].(float64))
			}
			if data["connectionTimeoutMs"] != nil {
				c.SocketConnectTimeout = time.Millisecond * time.Duration(data["connectionTimeoutMs"].(float64))
			}
		})
		if err != nil {
			b.writeError(err)
			return
		}
		idKey := b.nextId()
		b.drivers[idKey] = driver
		b.writeResponse("Driver", map[string]interface{}{"id": idKey})

	case "DriverClose":
		driverId := data["driverId"].(string)
		driver := b.drivers[driverId]
		err := driver.Close(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Driver", map[string]interface{}{"id": driverId})

	case "GetServerInfo":
		driverId := data["driverId"].(string)
		driver := b.drivers[driverId]
		serverInfo, err := driver.GetServerInfo(context.Background())
		if err != nil {
			b.writeError(err)
			return
		}
		protocolVersion := serverInfo.ProtocolVersion()
		b.writeResponse("ServerInfo", map[string]interface{}{
			"address":         serverInfo.Address(),
			"agent":           serverInfo.Agent(),
			"protocolVersion": fmt.Sprintf("%d.%d", protocolVersion.Major, protocolVersion.Minor),
		})

	case "NewSession":
		driver := b.drivers[data["driverId"].(string)]
		sessionConfig := neo4j.SessionConfig{
			BoltLogger: neo4j.ConsoleBoltLogger(),
		}
		switch data["accessMode"].(string) {
		case "r":
			sessionConfig.AccessMode = neo4j.AccessModeRead
		case "w":
			sessionConfig.AccessMode = neo4j.AccessModeWrite
		default:
			b.writeError(errors.New("Unknown access mode: " + data["accessMode"].(string)))
			return
		}
		if data["bookmarks"] != nil {
			rawBookmarks := data["bookmarks"].([]interface{})
			bookmarks := make([]string, len(rawBookmarks))
			for i, x := range rawBookmarks {
				bookmarks[i] = x.(string)
			}
			sessionConfig.Bookmarks = neo4j.BookmarksFromRawValues(bookmarks...)
		}
		if data["database"] != nil {
			sessionConfig.DatabaseName = data["database"].(string)
		}
		if data["fetchSize"] != nil {
			sessionConfig.FetchSize = int(data["fetchSize"].(float64))
		}
		if data["impersonatedUser"] != nil {
			sessionConfig.ImpersonatedUser = data["impersonatedUser"].(string)
		}
		session := driver.NewSession(ctx, sessionConfig)
		idKey := b.nextId()
		b.sessionStates[idKey] = &sessionState{session: session}
		b.writeResponse("Session", map[string]interface{}{"id": idKey})

	case "SessionClose":
		sessionId := data["sessionId"].(string)
		sessionState := b.sessionStates[sessionId]
		err := sessionState.session.Close(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Session", map[string]interface{}{"id": sessionId})

	case "SessionRun":
		sessionState := b.sessionStates[data["sessionId"].(string)]
		cypher, params := b.toCypherAndParams(data)
		result, err := sessionState.session.Run(ctx, cypher, params, b.toTransactionConfigApply(data))
		if err != nil {
			b.writeError(err)
			return
		}
		keys, err := result.Keys()
		if err != nil {
			b.writeError(err)
			return
		}
		idKey := b.nextId()
		b.results[idKey] = result
		b.writeResponse("Result", map[string]interface{}{"id": idKey, "keys": keys})

	case "SessionBeginTransaction":
		sessionState := b.sessionStates[data["sessionId"].(string)]
		tx, err := sessionState.session.BeginTransaction(ctx, b.toTransactionConfigApply(data))
		if err != nil {
			b.writeError(err)
			return
		}
		idKey := b.nextId()
		b.explicitTransactions[idKey] = tx
		b.writeResponse("Transaction", map[string]interface{}{"id": idKey})

	case "SessionLastBookmarks":
		sessionState := b.sessionStates[data["sessionId"].(string)]
		bookmarks := neo4j.BookmarksToRawValues(sessionState.session.LastBookmarks())
		if bookmarks == nil {
			bookmarks = []string{}
		}
		b.writeResponse("Bookmarks", map[string]interface{}{"bookmarks": bookmarks})

	case "TransactionRun":
		// ManagedTransaction is compatible with ExplicitTransaction
		// and is all that is needed for TransactionRun
		var tx neo4j.ManagedTransaction
		var found bool
		transactionId := data["txId"].(string)
		if tx, found = b.explicitTransactions[transactionId]; !found {
			tx = b.managedTransactions[transactionId]
		}
		cypher, params := b.toCypherAndParams(data)
		result, err := tx.Run(ctx, cypher, params)
		if err != nil {
			b.writeError(err)
			return
		}
		keys, err := result.Keys()
		if err != nil {
			b.writeError(err)
			return
		}
		idKey := b.nextId()
		b.results[idKey] = result
		b.writeResponse("Result", map[string]interface{}{"id": idKey, "keys": keys})

	case "TransactionCommit":
		txId := data["txId"].(string)
		tx := b.explicitTransactions[txId]
		err := tx.Commit(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Transaction", map[string]interface{}{"id": txId})

	case "TransactionRollback":
		txId := data["txId"].(string)
		tx := b.explicitTransactions[txId]
		err := tx.Rollback(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Transaction", map[string]interface{}{"id": txId})

	case "TransactionClose":
		txId := data["txId"].(string)
		tx := b.explicitTransactions[txId]
		err := tx.Close(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Transaction", map[string]interface{}{"id": txId})

	case "SessionReadTransaction":
		b.handleTransactionFunc(true, data)

	case "SessionWriteTransaction":
		b.handleTransactionFunc(false, data)

	case "RetryablePositive":
		sessionState := b.sessionStates[data["sessionId"].(string)]
		sessionState.retryableState = retryablePositive

	case "RetryableNegative":
		sessionState := b.sessionStates[data["sessionId"].(string)]
		sessionState.retryableState = retryableNegative
		sessionState.retryableErrorId = data["errorId"].(string)

	case "ResultNext":
		result := b.results[data["resultId"].(string)]
		more := result.Next(ctx)
		b.writeRecord(result, result.Record(), more)
	case "ResultPeek":
		result := b.results[data["resultId"].(string)]
		var record *db.Record = nil
		more := result.PeekRecord(ctx, &record)
		b.writeRecord(result, record, more)
	case "ResultList":
		result := b.results[data["resultId"].(string)]
		records, err := result.Collect(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		response := make([]interface{}, len(records))
		for i, record := range records {
			response[i] = serializeRecord(record)
		}
		b.writeResponse("RecordList", map[string]interface{}{
			"records": response,
		})
	case "ResultConsume":
		result := b.results[data["resultId"].(string)]
		summary, err := result.Consume(ctx)
		if err != nil {
			b.writeError(err)
			return
		}
		serverInfo := summary.Server()
		counters := summary.Counters()
		protocolVersion := serverInfo.ProtocolVersion()
		response := map[string]interface{}{
			"serverInfo": map[string]interface{}{
				"protocolVersion": fmt.Sprintf("%d.%d", protocolVersion.Major, protocolVersion.Minor),
				"agent":           serverInfo.Agent(),
				"address":         serverInfo.Address(),
			},
			"counters": map[string]interface{}{
				"constraintsAdded":      counters.ConstraintsAdded(),
				"constraintsRemoved":    counters.ConstraintsRemoved(),
				"containsSystemUpdates": counters.ContainsSystemUpdates(),
				"containsUpdates":       counters.ContainsUpdates(),
				"indexesAdded":          counters.IndexesAdded(),
				"indexesRemoved":        counters.IndexesRemoved(),
				"labelsAdded":           counters.LabelsAdded(),
				"labelsRemoved":         counters.LabelsRemoved(),
				"nodesCreated":          counters.NodesCreated(),
				"nodesDeleted":          counters.NodesDeleted(),
				"propertiesSet":         counters.PropertiesSet(),
				"relationshipsCreated":  counters.RelationshipsCreated(),
				"relationshipsDeleted":  counters.RelationshipsDeleted(),
				"systemUpdates":         counters.SystemUpdates(),
			},
			"query": map[string]interface{}{
				"text":       summary.Query().Text(),
				"parameters": serializeParameters(summary.Query().Parameters()),
			},
			"notifications": serializeNotifications(summary.Notifications()),
			"plan":          serializePlan(summary.Plan()),
			"profile":       serializeProfile(summary.Profile()),
		}
		if summary.ResultAvailableAfter() >= 0 {
			response["resultAvailableAfter"] = summary.ResultAvailableAfter().Milliseconds()
		}
		if summary.ResultConsumedAfter() >= 0 {
			response["resultConsumedAfter"] = summary.ResultConsumedAfter().Milliseconds()
		}
		if summary.StatementType() != neo4j.StatementTypeUnknown {
			response["queryType"] = summary.StatementType().String()
		}
		if summary.Database() != nil {
			response["database"] = summary.Database().Name()
		}
		b.writeResponse("Summary", response)

	case "CheckMultiDBSupport":
		driver := b.drivers[data["driverId"].(string)]
		session := driver.NewSession(ctx, neo4j.SessionConfig{
			BoltLogger: neo4j.ConsoleBoltLogger(),
		})
		result, err := session.Run(ctx, "RETURN 42", nil)
		defer func() {
			err = session.Close(ctx)
			if err != nil {
				b.writeError(fmt.Errorf("could not check multi DB support: %w", err))
			}
		}()
		if err != nil {
			b.writeError(fmt.Errorf("could not check multi DB support: %w", err))
			return
		}
		summary, err := result.Consume(ctx)
		if err != nil {
			b.writeError(fmt.Errorf("could not check multi DB support: %w", err))
			return
		}

		server := summary.Server()
		isMultiTenant := server.ProtocolVersion().Major >= 4
		b.writeResponse("MultiDBSupport", map[string]interface{}{
			"id":        b.nextId(),
			"available": isMultiTenant,
		})

	case "CheckDriverIsEncrypted":
		driver := b.drivers[data["driverId"].(string)]
		b.writeResponse("DriverIsEncrypted", map[string]interface{}{
			"encrypted": driver.IsEncrypted(),
		})

	case "VerifyConnectivity":
		driverId := data["driverId"].(string)
		if err := b.drivers[driverId].VerifyConnectivity(ctx); err != nil {
			b.writeError(err)
			return
		}
		b.writeResponse("Driver", map[string]interface{}{"id": driverId})

	case "GetFeatures":
		b.writeResponse("FeatureList", map[string]interface{}{
			"features": []string{
				"ConfHint:connection.recv_timeout_seconds",
				"Detail:ClosedDriverIsEncrypted",
				"Feature:API:ConnectionAcquisitionTimeout",
				"Feature:API:Driver:GetServerInfo",
				"Feature:API:Driver.IsEncrypted",
				"Feature:API:Driver.VerifyConnectivity",
				"Feature:API:Liveness.Check",
				"Feature:API:Result.List",
				"Feature:API:Result.Peek",
				"Feature:Auth:Custom",
				"Feature:Auth:Bearer",
				"Feature:Auth:Kerberos",
				"Feature:Bolt:3.0",
				"Feature:Bolt:4.1",
				"Feature:Bolt:4.2",
				"Feature:Bolt:4.3",
				"Feature:Bolt:4.4",
				"Feature:Bolt:5.0",
				"Feature:Impersonation",
				"Feature:TLS:1.1",
				"Feature:TLS:1.2",
				"Feature:TLS:1.3",
				"Optimization:ConnectionReuse",
				"Optimization:EagerTransactionBegin",
				"Optimization:ImplicitDefaultArguments",
				"Optimization:MinimalResets",
				"Optimization:PullPipelining",
			},
		})

	case "StartTest":
		testName := data["testName"].(string)
		if reason, ok := mustSkip(testName); ok {
			b.writeResponse("SkipTest", map[string]interface{}{"reason": reason})
			return
		}
		b.writeResponse("RunTest", nil)

	default:
		b.writeError(errors.New("Unknown request: " + name))
	}
}

func (b *backend) writeRecord(result neo4j.ResultWithContext, record *neo4j.Record, expectRecord bool) {
	if expectRecord && record == nil {
		b.writeResponse("BackendError", map[string]interface{}{
			"msg": "Found no record where one was expected.",
		})
	} else if !expectRecord && record != nil {
		b.writeResponse("BackendError", map[string]interface{}{
			"msg": "Found a record where none was expected.",
		})
	}

	if record != nil {
		b.writeResponse("Record", serializeRecord(record))
	} else {
		err := result.Err()
		if err != nil && err.Error() != "result cursor is not available anymore" {
			b.writeError(err)
			return
		}
		b.writeResponse("NullRecord", nil)
	}
}

func mustSkip(testName string) (string, bool) {
	skippedTests := testSkips()
	for testPattern, exclusionReason := range skippedTests {
		if matches(testPattern, testName) {
			return exclusionReason, true
		}
	}
	return "", false
}

func matches(pattern, testName string) bool {
	if pattern == testName {
		return true
	}
	if !strings.Contains(pattern, "*") {
		return false
	}
	regex := asRegex(pattern)
	return regex.MatchString(testName)
}

func asRegex(rawPattern string) *regexp.Regexp {
	pattern := regexp.QuoteMeta(rawPattern)
	pattern = strings.ReplaceAll(pattern, `\*`, ".*")
	return regexp.MustCompile(pattern)
}

func serializeRecord(record *neo4j.Record) map[string]interface{} {
	values := record.Values
	cypherValues := make([]interface{}, len(values))
	for i, v := range values {
		cypherValues[i] = nativeToCypher(v)
	}
	data := map[string]interface{}{"values": cypherValues}
	return data
}

func serializeNotifications(slice []neo4j.Notification) []map[string]interface{} {
	if slice == nil {
		return nil
	}
	if len(slice) == 0 {
		return []map[string]interface{}{}
	}
	var res []map[string]interface{}
	for i, notification := range slice {
		res = append(res, map[string]interface{}{
			"code":        notification.Code(),
			"title":       notification.Title(),
			"description": notification.Description(),
			"severity":    notification.Severity(),
		})
		if notification.Position() != nil {
			res[i]["position"] = map[string]interface{}{
				"offset": notification.Position().Offset(),
				"line":   notification.Position().Line(),
				"column": notification.Position().Column(),
			}
		}
	}
	return res
}

func serializePlan(plan neo4j.Plan) map[string]interface{} {
	if plan == nil {
		return nil
	}
	return map[string]interface{}{
		"args":         plan.Arguments(),
		"operatorType": plan.Operator(),
		"children":     serializePlans(plan.Children()),
		"identifiers":  plan.Identifiers(),
	}
}

func serializePlans(children []neo4j.Plan) []map[string]interface{} {
	result := make([]map[string]interface{}, len(children))
	for i, child := range children {
		result[i] = serializePlan(child)
	}
	return result
}

func serializeProfile(profile neo4j.ProfiledPlan) map[string]interface{} {
	if profile == nil {
		return nil
	}
	result := map[string]interface{}{
		"args":         profile.Arguments(),
		"children":     serializeProfiles(profile.Children()),
		"dbHits":       profile.DbHits(),
		"identifiers":  profile.Identifiers(),
		"operatorType": profile.Operator(),
		"rows":         profile.Records(),
	}
	return result
}

func serializeProfiles(children []neo4j.ProfiledPlan) []map[string]interface{} {
	result := make([]map[string]interface{}, len(children))
	for i, child := range children {
		childProfile := serializeProfile(child)
		childProfile["pageCacheMisses"] = child.PageCacheMisses()
		childProfile["pageCacheHits"] = child.PageCacheHits()
		childProfile["pageCacheHitRatio"] = child.PageCacheHitRatio()
		childProfile["time"] = child.Time()
		result[i] = childProfile
	}
	return result
}

func serializeParameters(parameters map[string]interface{}) map[string]interface{} {
	result := make(map[string]interface{}, len(parameters))
	for k, parameter := range parameters {
		result[k] = nativeToCypher(parameter)
	}
	return result
}

// you can use '*' as wildcards anywhere in the qualified test name (useful to exclude a whole class e.g.)
func testSkips() map[string]string {
	return map[string]string{
		"stub.disconnects.test_disconnects.TestDisconnects.test_fail_on_reset":                                                   "It is not resetting driver when put back to pool",
		"stub.routing.test_routing_v3.RoutingV3.test_should_use_resolver_during_rediscovery_when_existing_routers_fail":          "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x1.RoutingV4x1.test_should_use_resolver_during_rediscovery_when_existing_routers_fail":      "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x3.RoutingV4x3.test_should_use_resolver_during_rediscovery_when_existing_routers_fail":      "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x4.RoutingV4x4.test_should_use_resolver_during_rediscovery_when_existing_routers_fail":      "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v5x0.RoutingV5x0.test_should_use_resolver_during_rediscovery_when_existing_routers_fail":      "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v3.RoutingV3.test_should_revert_to_initial_router_if_known_router_throws_protocol_errors":     "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x1.RoutingV4x1.test_should_revert_to_initial_router_if_known_router_throws_protocol_errors": "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x3.RoutingV4x3.test_should_revert_to_initial_router_if_known_router_throws_protocol_errors": "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v4x4.RoutingV4x4.test_should_revert_to_initial_router_if_known_router_throws_protocol_errors": "It needs investigation - custom resolver does not seem to be called",
		"stub.routing.test_routing_v5x0.RoutingV5x0.test_should_revert_to_initial_router_if_known_router_throws_protocol_errors": "It needs investigation - custom resolver does not seem to be called",
		"stub.configuration_hints.test_connection_recv_timeout_seconds.TestRoutingConnectionRecvTimeout.*":                       "No GetRoutingTable support - too tricky to implement in Go",
		"stub.homedb.test_homedb.TestHomeDb.test_session_should_cache_home_db_despite_new_rt":                                    "Driver does not remove servers from RT when connection breaks.",
		"stub.iteration.test_result_scope.TestResultScope.*":                                                                     "Results are always valid but don't return records when out of scope",
		"stub.*.test_0_timeout":        "Driver omits 0 as tx timeout value",
		"stub.*.test_negative_timeout": "Driver omits negative tx timeout values",
		"stub.routing.*.*.test_should_request_rt_from_all_initial_routers_until_successful_on_unknown_failure":                              "Add DNS resolver TestKit message and connection timeout support",
		"stub.routing.*.*.test_should_request_rt_from_all_initial_routers_until_successful_on_authorization_expired":                        "Add DNS resolver TestKit message and connection timeout support",
		"stub.summary.test_summary.TestSummary.test_server_info":                                                                            "Needs some kind of server address DNS resolution",
		"stub.summary.test_summary.TestSummary.test_invalid_query_type":                                                                     "Driver does not verify query type returned from server.",
		"stub.routing.*.test_should_drop_connections_failing_liveness_check":                                                                "Needs support for GetConnectionPoolMetrics",
		"stub.connectivity_check.test_get_server_info.TestGetServerInfo.test_routing_fail_when_no_reader_are_available":                     "Won't fix - Go driver retries routing table when no readers are available",
		"stub.connectivity_check.test_verify_connectivity.TestVerifyConnectivity.test_routing_fail_when_no_reader_are_available":            "Won't fix - Go driver retries routing table when no readers are available",
		"stub.driver_parameters.test_connection_acquisition_timeout_ms.TestConnectionAcquisitionTimeoutMs.test_does_not_encompass_router_*": "Won't fix - ConnectionAcquisitionTimeout spans the whole process including db resolution, RT updates, connection acquisition from the pool, and creation of new connections.",
	}
}
