package wifi

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

const (
	// replySize is wpa_supplicant's control reply buffer.
	replySize = 4096

	// requestTimeout bounds a request without an earlier context deadline.
	requestTimeout = 5 * time.Second

	// scanWait bounds the wait for the results of a scan.
	scanWait = 15 * time.Second

	// bssMask asks for id, bssid, freq, level, ie, flags and ssid.
	bssMask = "0x1c87"

	// bssMaskNoIEs asks for the same fields without the information elements, so an
	// entry too large for one reply still fits.
	bssMaskNoIEs = "0x1887"

	// maxBSS bounds one walk of the scan results.
	maxBSS = 1024
)

var dialSeq atomic.Uint64

// Conn is a client of one wpa_supplicant control socket (AF_UNIX datagrams).
type Conn struct {
	conn     *net.UnixConn
	local    string
	attached bool
	events   []string
}

// Dial connects to the control socket of iface in ctrlDir, from a socket bound in
// ctrlDir that Close removes.
func Dial(ctrlDir string, iface string) (*Conn, error) {
	local := filepath.Join(ctrlDir, fmt.Sprintf(".osd-%d-%d", os.Getpid(), dialSeq.Add(1)))
	_ = os.Remove(local)

	conn, err := net.DialUnix("unixgram", &net.UnixAddr{Name: local, Net: "unixgram"}, &net.UnixAddr{Name: filepath.Join(ctrlDir, iface), Net: "unixgram"})
	if err != nil {
		_ = os.Remove(local)

		return nil, err
	}

	return &Conn{conn: conn, local: local}, nil
}

func deadline(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)

	ctxDeadline, ok := ctx.Deadline()
	if ok && ctxDeadline.Before(t) {
		return ctxDeadline
	}

	return t
}

// Request sends cmd and returns the reply. Unsolicited messages that arrive first are kept for Event.
func (c *Conn) Request(ctx context.Context, cmd string) (string, error) {
	err := c.conn.SetDeadline(deadline(ctx, requestTimeout))
	if err != nil {
		return "", err
	}

	_, err = c.conn.Write([]byte(cmd))
	if err != nil {
		return "", err
	}

	buf := make([]byte, replySize)

	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return "", err
		}

		msg := string(buf[:n])
		if c.attached && strings.HasPrefix(msg, "<") {
			c.events = append(c.events, stripPriority(msg))

			continue
		}

		return msg, nil
	}
}

// Attach subscribes to the supplicant's unsolicited messages.
func (c *Conn) Attach(ctx context.Context) error {
	reply, err := c.Request(ctx, "ATTACH")
	if err != nil {
		return err
	}

	if strings.TrimSpace(reply) != "OK" {
		return errors.New("ATTACH: " + strings.TrimSpace(reply))
	}

	c.attached = true

	return nil
}

// Event returns the next unsolicited message without its "<N>" priority, waiting
// until the context's deadline, or 5 s without one.
func (c *Conn) Event(ctx context.Context) (string, error) {
	if len(c.events) > 0 {
		ev := c.events[0]
		c.events = c.events[1:]

		return ev, nil
	}

	t := time.Now().Add(requestTimeout)

	ctxDeadline, ok := ctx.Deadline()
	if ok {
		t = ctxDeadline
	}

	err := c.conn.SetReadDeadline(t)
	if err != nil {
		return "", err
	}

	buf := make([]byte, replySize)

	for {
		n, err := c.conn.Read(buf)
		if err != nil {
			return "", err
		}

		msg := string(buf[:n])
		if strings.HasPrefix(msg, "<") {
			return stripPriority(msg), nil
		}
	}
}

// Close detaches, closes the socket and removes its file.
func (c *Conn) Close() error {
	if c.attached {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		_, _ = c.Request(ctx, "DETACH")

		cancel()
	}

	err := c.conn.Close()
	_ = os.Remove(c.local)

	return err
}

func stripPriority(msg string) string {
	if strings.HasPrefix(msg, "<") {
		_, rest, ok := strings.Cut(msg, ">")
		if ok {
			return rest
		}
	}

	return msg
}

// BSS is one scan result. NoIEs marks an entry that was read without its
// information elements, because the entry with them did not fit in one reply.
type BSS struct {
	ID    int
	BSSID string
	Freq  int
	Level int
	Flags string
	SSID  string
	IEs   []byte

	NoIEs bool
}

// Scan asks for a scan, waits up to 15 s for its results, and walks them one BSS
// per request, because each reply is capped at 4096 bytes. An entry too large for a
// reply makes the supplicant send nothing for it, which looks like the end of the
// walk; BSS LAST shows that entries remain, and that entry is then read without its
// information elements and the walk goes on from it.
func (c *Conn) Scan(ctx context.Context) ([]BSS, error) {
	if !c.attached {
		err := c.Attach(ctx)
		if err != nil {
			return nil, err
		}
	}

	reply, err := c.Request(ctx, "SCAN")
	if err != nil {
		return nil, err
	}

	if !strings.HasPrefix(reply, "OK") && !strings.HasPrefix(reply, "FAIL-BUSY") {
		return nil, errors.New("SCAN: " + strings.TrimSpace(reply))
	}

	// Without the results event in time, the walk reads what the supplicant has.
	waitCtx, cancel := context.WithTimeout(ctx, scanWait)
	for {
		ev, err := c.Event(waitCtx)
		if err != nil || strings.HasPrefix(ev, "CTRL-EVENT-SCAN-RESULTS") || strings.HasPrefix(ev, "CTRL-EVENT-SCAN-FAILED") {
			break
		}
	}

	cancel()

	results := []BSS{}
	mask := bssMask

	for len(results) < maxBSS {
		reply, err := c.Request(ctx, nextCmd(results, mask))
		if err != nil {
			return nil, err
		}

		if reply == "" {
			// The end of the walk, or an entry that did not fit. Without IEs an entry
			// fits, so a second empty reply is the end.
			if mask == bssMaskNoIEs {
				break
			}

			skipped, err := c.skipped(ctx, results)
			if err != nil {
				return nil, err
			}

			if !skipped {
				break
			}

			mask = bssMaskNoIEs

			continue
		}

		bss, err := parseBSS(reply)
		if err != nil {
			return nil, err
		}

		bss.NoIEs = mask == bssMaskNoIEs
		results = append(results, bss)
		mask = bssMask
	}

	return results, nil
}

// nextCmd asks for the BSS after the last one read, or the first one.
func nextCmd(results []BSS, mask string) string {
	if len(results) == 0 {
		return "BSS FIRST MASK=" + mask
	}

	return fmt.Sprintf("BSS NEXT-%d MASK=%s", results[len(results)-1].ID, mask)
}

// skipped reports whether the supplicant holds a BSS after the last one read. BSS
// LAST names the newest BSS; a different id than the last one read means an entry
// was not read. It reports false when the supplicant holds no BSS at all.
func (c *Conn) skipped(ctx context.Context, results []BSS) (bool, error) {
	reply, err := c.Request(ctx, "BSS LAST MASK=0x1")
	if err != nil {
		return false, err
	}

	id, ok := lastID(reply)
	if !ok {
		return false, nil
	}

	if len(results) == 0 {
		return true, nil
	}

	return id != results[len(results)-1].ID, nil
}

// lastID reads the "id=" line of a BSS LAST reply.
func lastID(reply string) (int, bool) {
	for line := range strings.SplitSeq(reply, "\n") {
		if v, ok := strings.CutPrefix(line, "id="); ok {
			id, err := strconv.Atoi(strings.TrimSpace(v))

			return id, err == nil
		}
	}

	return 0, false
}

func parseBSS(reply string) (BSS, error) {
	bss := BSS{}
	hasID := false

	for line := range strings.SplitSeq(reply, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}

		switch key {
		case "id":
			id, err := strconv.Atoi(value)
			if err == nil {
				bss.ID = id
				hasID = true
			}
		case "bssid":
			bss.BSSID = value
		case "freq":
			bss.Freq, _ = strconv.Atoi(value)
		case "level":
			bss.Level, _ = strconv.Atoi(value)
		case "ie":
			ies, err := hex.DecodeString(value)
			if err == nil {
				bss.IEs = ies
			}
		case "flags":
			bss.Flags = value
		case "ssid":
			bss.SSID = unescapeSSID(value)
		default:
			// A field the scan does not use.
		}
	}

	if !hasID {
		return bss, errors.New("BSS: a reply without an id")
	}

	return bss, nil
}

// unescapeSSID undoes wpa_supplicant's printf_encode of an SSID.
func unescapeSSID(s string) string {
	out := make([]byte, 0, len(s))

	for i := 0; i < len(s); i++ {
		if s[i] != '\\' || i+1 == len(s) {
			out = append(out, s[i])

			continue
		}

		i++

		switch s[i] {
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'e':
			out = append(out, 0x1b)
		case 'x':
			v, err := hex.DecodeString(s[i+1 : min(i+3, len(s))])
			if err != nil || len(v) != 1 {
				out = append(out, '\\', 'x')

				continue
			}

			out = append(out, v[0])
			i += 2
		default:
			out = append(out, s[i])
		}
	}

	return string(out)
}
