// Contains all commands callable by a client
package commands

import (
	"fmt"
	"github.com/fzzy/radix/redis"
	"github.com/fzzy/radix/redis/resp"
	"io"
	"strconv"
	"strings"

	"github.com/mc0/redeque/clients"
	"github.com/mc0/redeque/db"
)

var commandMap = map[string]func(*clients.Client, []string) error{
	"QREGISTER": qregister,
	"QRPOP":     qrpop,
	"QLPEEK":    qlpeek,
	"QRPEEK":    qrpeek,
	"QREM":      qrem,
	"QLPUSH":    qlpush,
	"QRPUSH":    qrpush,
	"QSTATUS":   qstatus,
	"PING":      ping,
}

// All commands take in a client whose command has already been read off the
// socket, a list of arguments from that command (not including the command name
// itself), and return an error ONLY if the error is worth logging (disconnect
// from redis, etc...)
func Dispatch(client *clients.Client, cmd string, args []string) error {
	if len(cmd) == 0 {
		writeErrf(client.Conn, "ERR no command found")
		return nil
	}

	cmdFunc, ok := commandMap[cmd]
	if !ok {
		return unknown(client, cmd)
	}

	return cmdFunc(client, args)
}

func writeServerErr(w io.Writer, err error) error {
	return writeErrf(w, "ERR server-side: %s", err)
}

func writeErrf(w io.Writer, format string, args ...interface{}) error {
	err := fmt.Errorf(format, args...)
	return resp.WriteArbitrary(w, err)
}

func qregister(client *clients.Client, args []string) error {
	err := client.UpdateQueues(args)
	if err != nil {
		writeServerErr(client.Conn, err)
		return err
	}

	conn := client.Conn

	// TODO add WriteSimpleString to resp
	conn.Write([]byte("+OK\r\n"))
	return nil
}

func qlpeek(client *clients.Client, args []string) error {
	return qpeekgeneric(client, args, false)
}

func qrpeek(client *clients.Client, args []string) error {
	return qpeekgeneric(client, args, true)
}

func qpeekgeneric(client *clients.Client, args []string, peekRight bool) error {
	conn := client.Conn
	if len(args) < 1 {
		writeErrf(conn, "ERR missing args")
		return nil
	}

	queueName := args[0]

	redisClient, err := db.RedisPool.Get()
	if err != nil {
		writeServerErr(conn, err)
		return fmt.Errorf("QPEEK* redisPool.Get(): %s", err)
	}

	unclaimedKey := db.UnclaimedKey(queueName)
	offset := "0"
	if peekRight {
		offset = "-1"
	}
	reply := redisClient.Cmd("LRANGE", unclaimedKey, offset, offset)
	if reply.Type == redis.NilReply {
		resp.WriteArbitrary(conn, nil)
		return nil
	}

	eventIDs, err := reply.List()
	if err != nil {
		resp.WriteArbitrary(conn, nil)
		return fmt.Errorf("QPEEK* LRANGE: %s", err)
	}

	if len(eventIDs) == 0 {
		resp.WriteArbitrary(conn, eventIDs)
		return nil
	}

	eventID := eventIDs[0]

	itemsKey := db.ItemsKey(queueName)
	reply = redisClient.Cmd("HGET", itemsKey, eventID)

	var eventRaw string
	if reply.Type != redis.NilReply {
		if eventRaw, err = reply.Str(); err != nil {
			writeServerErr(conn, err)
			return fmt.Errorf("QPEEK* HGET: %s", err)
		}
	}

	resp.WriteArbitrary(conn, []string{eventID, eventRaw})
	db.RedisPool.Put(redisClient)
	return nil
}

func qrpop(client *clients.Client, args []string) error {
	conn := client.Conn
	if len(args) < 1 {
		writeErrf(conn, "ERR missing args")
		return nil
	}

	queueName := args[0]
	var err error
	expires := 30

	unsafe := false
	if len(args) > 2 && strings.ToUpper(args[1]) == "EX" {
		expires, err = strconv.Atoi(args[2])
		if err != nil {
			writeErrf(conn, "ERR bad expires value: %s", err)
			return nil
		}
		args = args[3:]
	}
	if len(args) > 0 && strings.ToUpper(args[0]) == "UNSAFE" {
		unsafe = true
	}

	redisClient, err := db.RedisPool.Get()
	if err != nil {
		writeServerErr(conn, err)
		return fmt.Errorf("QRPOP redisPool.Get(): %s", err)
	}

	unclaimedKey := db.UnclaimedKey(queueName)
	claimedKey := db.ClaimedKey(queueName)
	reply := redisClient.Cmd("RPOPLPUSH", unclaimedKey, claimedKey)
	if reply.Type == redis.NilReply {
		resp.WriteArbitrary(conn, nil)
		return nil
	}

	eventID, err := reply.Str()
	if err != nil {
		writeServerErr(conn, err)
		return fmt.Errorf("QRPOP RPOPLPUSH: %s", err)
	}

	lockKey := db.ItemLockKey(queueName, eventID)
	reply = redisClient.Cmd("SET", lockKey, 1, "EX", strconv.Itoa(expires), "NX")
	if reply.Err != nil {
		writeServerErr(conn, reply.Err)
		return fmt.Errorf("QRPOP SET: %s", reply.Err)
	}

	itemsKey := db.ItemsKey(queueName)
	reply = redisClient.Cmd("HGET", itemsKey, eventID)

	var eventRaw string
	if reply.Type != redis.NilReply {
		if eventRaw, err = reply.Str(); err != nil {
			writeServerErr(conn, err)
			return fmt.Errorf("QRPOP HGET: %s", err)
		}
	}

	if unsafe {
		qrem(client, []string{queueName, eventID})
	}

	resp.WriteArbitrary(conn, []string{eventID, eventRaw})
	db.RedisPool.Put(redisClient)
	return nil
}

func qrem(client *clients.Client, args []string) error {
	var err error
	conn := client.Conn
	if len(args) < 2 {
		writeErrf(conn, "ERR missing args")
		return nil
	}
	redisClient, err := db.RedisPool.Get()
	if err != nil {
		writeServerErr(conn, err)
		return err
	}

	queueName, eventID := args[0], args[1]
	claimedKey := db.ClaimedKey(queueName)
	itemsKey := db.ItemsKey(queueName)
	lockKey := db.ItemLockKey(queueName, eventID)

	redisClient.Append("LREM", claimedKey, -1, eventID)
	redisClient.Append("HDEL", itemsKey, eventID)
	redisClient.Append("DEL", lockKey)
	var numRemoved int
	for i := 0; i < 3; i++ {
		if reply := redisClient.GetReply(); reply.Err != nil {
			writeServerErr(conn, err)
			return fmt.Errorf("QREM %d: %s", i, reply.Err)
			// reply from LREM
		} else if i == 0 {
			numRemoved, _ = reply.Int()
		}
	}

	resp.WriteArbitrary(conn, numRemoved)
	db.RedisPool.Put(redisClient)
	return nil
}

func qlpush(client *clients.Client, args []string) error {
	return qpushgeneric(client, args, false)
}

func qrpush(client *clients.Client, args []string) error {
	return qpushgeneric(client, args, true)
}

func qpushgeneric(client *clients.Client, args []string, pushRight bool) error {
	conn := client.Conn
	if len(args) < 3 {
		writeErrf(conn, "ERR missing args")
		return nil
	}

	redisClient, err := db.RedisPool.Get()
	if err != nil {
		writeServerErr(conn, err)
		return err
	}

	queueName, eventID, contents := args[0], args[1], args[2]
	itemsKey := db.ItemsKey(queueName)

	created, err := redisClient.Cmd("HSETNX", itemsKey, eventID, contents).Int()
	if err != nil {
		writeServerErr(conn, err)
		return fmt.Errorf("QPUSH* HSETNX: %s", err)
	} else if created == 0 {
		writeErrf(conn, "ERR duplicate event %q", eventID)
		db.RedisPool.Put(redisClient)
		return nil
	}

	unclaimedKey := db.UnclaimedKey(queueName)
	cmd := "LPUSH"
	if pushRight {
		cmd = "RPUSH"
	}
	if err := redisClient.Cmd(cmd, unclaimedKey, eventID).Err; err != nil {
		writeServerErr(conn, err)
		return fmt.Errorf("QPUSH* %s: %s", cmd, err)
	}

	// TODO resp simple string
	conn.Write([]byte("+OK\r\n"))
	db.RedisPool.Put(redisClient)
	return nil
}

func qstatus(client *clients.Client, args []string) error {
	conn := client.Conn

	redisClient, err := db.RedisPool.Get()
	if err != nil {
		writeServerErr(conn, err)
		return err
	}

	queueNames, err := db.AllQueueNames(redisClient)
	if err != nil {
		writeServerErr(conn, err)
		return err
	}

	var queueStatuses []string

	for i := range queueNames {
		queueName := queueNames[i]
		claimedCount := 0
		availableCount := 0
		totalCount := 0

		unclaimedKey := db.UnclaimedKey(queueName)
		claimedKey := db.ClaimedKey(queueName)

		claimedCount, err = redisClient.Cmd("LLEN", claimedKey).Int()
		if err != nil {
			writeServerErr(conn, err)
			return fmt.Errorf("QSTATUS LLEN claimed: %s", err)
		}

		availableCount, err = redisClient.Cmd("LLEN", unclaimedKey).Int()
		if err != nil {
			writeServerErr(conn, err)
			return fmt.Errorf("QSTATUS LLEN unclaimed): %s", err)
		}

		totalCount = availableCount + claimedCount

		queueStatus := fmt.Sprintf("%s %d %d", queueName, totalCount, claimedCount)
		queueStatuses = append(queueStatuses, queueStatus)
	}

	resp.WriteArbitrary(conn, queueStatuses)
	db.RedisPool.Put(redisClient)
	return nil
}

func ping(client *clients.Client, args []string) error {
	conn := client.Conn
	if len(args) != 0 {
		writeErrf(conn, "ERR wrong arg count")
		return nil
	}

	// TODO add WriteSimpleString to resp
	conn.Write([]byte("+PONG\r\n"))
	return nil
}

func unknown(client *clients.Client, command string) error {
	writeErrf(client.Conn, "ERR unknown command '%s`", command)
	return nil
}