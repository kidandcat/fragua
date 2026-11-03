//go:build lean

package leanbridge

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
)

// RPCCalls counts completed requests to the geometry helper. Tests use it to
// show that a real board actually exercised the Lean implementation.
var RPCCalls atomic.Uint64

const maxFrame = 8 << 20

type rpcReq struct {
	Op        string   `json:"op"`
	Ax        string   `json:"ax,omitempty"`
	Ay        string   `json:"ay,omitempty"`
	Bx        string   `json:"bx,omitempty"`
	By        string   `json:"by,omitempty"`
	Px        string   `json:"px,omitempty"`
	Py        string   `json:"py,omitempty"`
	A0x       string   `json:"a0x,omitempty"`
	A0y       string   `json:"a0y,omitempty"`
	A1x       string   `json:"a1x,omitempty"`
	A1y       string   `json:"a1y,omitempty"`
	B0x       string   `json:"b0x,omitempty"`
	B0y       string   `json:"b0y,omitempty"`
	B1x       string   `json:"b1x,omitempty"`
	B1y       string   `json:"b1y,omitempty"`
	Num       string   `json:"num,omitempty"`
	Den       string   `json:"den,omitempty"`
	Clearance string   `json:"clearance,omitempty"`
	N         string   `json:"n,omitempty"`
	Items     []rpcReq `json:"items,omitempty"`
}

type rpcResp struct {
	Num   string    `json:"num"`
	Den   string    `json:"den"`
	OK    *bool     `json:"ok"`
	Pong  *bool     `json:"pong"`
	Err   string    `json:"err"`
	Items []rpcResp `json:"items"`
}

type geomClient struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	w   *bufio.Writer
	r   *bufio.Reader
}

var (
	startOnce sync.Once
	startErr  error
	client    *geomClient
)

func geomBin() (string, error) {
	if p := os.Getenv("FRAGUA_GEOM_BIN"); p != "" {
		st, err := os.Stat(p)
		if err != nil {
			return "", fmt.Errorf("FRAGUA_GEOM_BIN=%s: %w", p, err)
		}
		if st.IsDir() {
			return "", fmt.Errorf("FRAGUA_GEOM_BIN=%s is a directory", p)
		}
		return p, nil
	}
	names := []string{"fragua-geom"}
	if runtime.GOOS == "windows" {
		names = []string{"fragua-geom.exe", "fragua-geom"}
	}
	var dirs []string
	if cwd, err := os.Getwd(); err == nil {
		dirs = append(dirs, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	for _, start := range dirs {
		dir := start
		for {
			for _, name := range names {
				cand := filepath.Join(dir, "lean", ".lake", "build", "bin", name)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand, nil
				}
				cand = filepath.Join(dir, name)
				if st, err := os.Stat(cand); err == nil && !st.IsDir() {
					return cand, nil
				}
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", fmt.Errorf("fragua-geom not found; set FRAGUA_GEOM_BIN or run `lake build` in lean/")
}

func startClient() (*geomClient, error) {
	bin, err := geomBin()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", bin, err)
	}
	c := &geomClient{
		cmd: cmd,
		w:   bufio.NewWriterSize(stdin, 64*1024),
		r:   bufio.NewReaderSize(stdout, 64*1024),
	}
	var pong rpcResp
	if err := c.roundTrip(rpcReq{Op: "ping"}, &pong); err != nil {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("fragua-geom ping: %w", err)
	}
	if pong.Pong == nil || !*pong.Pong {
		_ = cmd.Process.Kill()
		return nil, fmt.Errorf("fragua-geom ping: unexpected response")
	}
	return c, nil
}

func getClient() (*geomClient, error) {
	startOnce.Do(func() {
		client, startErr = startClient()
	})
	if startErr != nil {
		return nil, startErr
	}
	return client, nil
}

func (c *geomClient) roundTrip(req rpcReq, resp *rpcResp) error {
	payload, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if len(payload) > maxFrame {
		return fmt.Errorf("request is %d bytes", len(payload))
	}
	var hdr [4]byte
	binary.LittleEndian.PutUint32(hdr[:], uint32(len(payload)))
	if _, err := c.w.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := c.w.Write(payload); err != nil {
		return err
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	if _, err := io.ReadFull(c.r, hdr[:]); err != nil {
		return err
	}
	n := binary.LittleEndian.Uint32(hdr[:])
	if n > maxFrame {
		return fmt.Errorf("response is %d bytes", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.r, buf); err != nil {
		return err
	}
	if err := json.Unmarshal(buf, resp); err != nil {
		return fmt.Errorf("decode response: %w (%s)", err, buf)
	}
	if resp.Err != "" {
		return fmt.Errorf("%s", resp.Err)
	}
	RPCCalls.Add(1)
	return nil
}

func call(req rpcReq) (rpcResp, error) {
	c, err := getClient()
	if err != nil {
		return rpcResp{}, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var resp rpcResp
	if err := c.roundTrip(req, &resp); err != nil {
		return rpcResp{}, err
	}
	return resp, nil
}

func parseRat(resp rpcResp) (Rat, error) {
	num, ok := new(big.Int).SetString(resp.Num, 10)
	if !ok {
		return Rat{}, fmt.Errorf("bad numerator %q", resp.Num)
	}
	den, ok := new(big.Int).SetString(resp.Den, 10)
	if !ok {
		return Rat{}, fmt.Errorf("bad denominator %q", resp.Den)
	}
	return Rat{Num: num, Den: den}, nil
}

func i64s(v int64) string {
	return new(big.Int).SetInt64(v).String()
}

func leanDist2(ax, ay, bx, by int64) (*big.Int, error) {
	resp, err := call(rpcReq{Op: "dist2", Ax: i64s(ax), Ay: i64s(ay), Bx: i64s(bx), By: i64s(by)})
	if err != nil {
		return nil, err
	}
	r, err := parseRat(resp)
	if err != nil {
		return nil, err
	}
	if r.Den.Cmp(big.NewInt(1)) != 0 {
		return nil, fmt.Errorf("dist2 denominator %s", r.Den)
	}
	return r.Num, nil
}

func leanPointSeg(px, py, ax, ay, bx, by int64) (Rat, error) {
	resp, err := call(rpcReq{
		Op: "point_seg",
		Px: i64s(px), Py: i64s(py),
		Ax: i64s(ax), Ay: i64s(ay),
		Bx: i64s(bx), By: i64s(by),
	})
	if err != nil {
		return Rat{}, err
	}
	return parseRat(resp)
}

func leanSegSeg(a0x, a0y, a1x, a1y, b0x, b0y, b1x, b1y int64) (Rat, error) {
	resp, err := call(rpcReq{
		Op:  "seg_seg",
		A0x: i64s(a0x), A0y: i64s(a0y),
		A1x: i64s(a1x), A1y: i64s(a1y),
		B0x: i64s(b0x), B0y: i64s(b0y),
		B1x: i64s(b1x), B1y: i64s(b1y),
	})
	if err != nil {
		return Rat{}, err
	}
	return parseRat(resp)
}

func leanSeparated(num, den *big.Int, clearance int64) (bool, error) {
	resp, err := call(rpcReq{
		Op:        "separated",
		Num:       num.String(),
		Den:       den.String(),
		Clearance: i64s(clearance),
	})
	if err != nil {
		return false, err
	}
	if resp.OK == nil {
		return false, fmt.Errorf("separated response missing ok")
	}
	return *resp.OK, nil
}

func leanNorm(num, den *big.Int) (Rat, error) {
	resp, err := call(rpcReq{Op: "norm", Num: num.String(), Den: den.String()})
	if err != nil {
		return Rat{}, err
	}
	return parseRat(resp)
}

// HelperPID is the geometry process. Zero means it has not been started.
func HelperPID() int {
	c, err := getClient()
	if err != nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// leanSegSum runs n segment distances inside the helper and returns the sum
// of the normalized numerators. One frame covers the whole loop.
func leanSegSum(n int) (*big.Int, error) {
	resp, err := call(rpcReq{Op: "seg_sum", N: i64s(int64(n))})
	if err != nil {
		return nil, err
	}
	r, err := parseRat(resp)
	if err != nil {
		return nil, err
	}
	if r.Den.Cmp(big.NewInt(1)) != 0 {
		return nil, fmt.Errorf("seg_sum denominator %s", r.Den)
	}
	return r.Num, nil
}

// leanSegSegBatch evaluates many segment distances in one round trip.
func leanSegSegBatch(segs [][8]int64) ([]Rat, error) {
	if len(segs) == 0 {
		return nil, nil
	}
	const chunk = 256
	out := make([]Rat, 0, len(segs))
	for i := 0; i < len(segs); i += chunk {
		j := i + chunk
		if j > len(segs) {
			j = len(segs)
		}
		items := make([]rpcReq, 0, j-i)
		for _, s := range segs[i:j] {
			items = append(items, rpcReq{
				Op:  "seg_seg",
				A0x: i64s(s[0]), A0y: i64s(s[1]),
				A1x: i64s(s[2]), A1y: i64s(s[3]),
				B0x: i64s(s[4]), B0y: i64s(s[5]),
				B1x: i64s(s[6]), B1y: i64s(s[7]),
			})
		}
		resp, err := call(rpcReq{Op: "batch", Items: items})
		if err != nil {
			return nil, err
		}
		if len(resp.Items) != len(items) {
			return nil, fmt.Errorf("batch returned %d items, want %d", len(resp.Items), len(items))
		}
		for _, item := range resp.Items {
			if item.Err != "" {
				return nil, fmt.Errorf("%s", item.Err)
			}
			r, err := parseRat(item)
			if err != nil {
				return nil, err
			}
			out = append(out, r)
		}
	}
	return out, nil
}
