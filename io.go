package tapfs

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	iofs "io/fs"
	"log"
	"net"
	"net/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

// CommandServer serves RPC calls
type CommandServer struct {
	root   *TapFSRoot
	depDir string

	mu       sync.Mutex
	Debug    bool
	debugLog io.WriteCloser
}

const socketName = ".tapfs"

func StartCommandServer(root *TapFSRoot, depDir string, server *fuse.Server, Debug bool) error {
	server.WaitMount()
	l, sock, err := newSocket()
	if err != nil {
		return err
	}

	ch := root.NewPersistentInode(context.Background(), &fs.MemSymlink{
		Data: []byte(sock),
	}, fs.StableAttr{Mode: fuse.S_IFLNK})
	root.AddChild(socketName, ch, true)

	commandServer := &CommandServer{
		root:   root,
		depDir: depDir,
		Debug:  Debug,
	}
	srv := rpc.NewServer()
	if err := srv.Register(commandServer); err != nil {
		return err
	}
	go srv.Accept(l)

	return nil
}

func FindSocket(startDir string) (socket string, topdir string, err error) {
	for dir := startDir; dir != "/"; dir = filepath.Dir(dir) {
		p := filepath.Join(dir, socketName)
		fi, err := os.Stat(p)
		if fi == nil || fi.Mode()&iofs.ModeType != iofs.ModeSocket {
			continue
		}
		val, err := os.Readlink(p)
		if err != nil {
			return "", "", err
		}
		if filepath.IsAbs(val) {
			return val, dir, nil
		}
	}

	return "", "", fmt.Errorf("socket %q not found", socketName)
}

type TraceRequest struct {
	PGID int
}

type TraceResponse struct {
	// only populated for EndTrace
	ID     string
	DepDir string
	Read   []string
	Create []string
	Update []string
	Delete []string
}

type JSONOpenData struct {
	ID      string
	Command string
	Dir     string

	Read   []string
	Create []string
	Update []string
	Delete []string
}

func (s *CommandServer) StartTrace(req *TraceRequest, rep *TraceResponse) error {
	od := s.root.registerPGID(req.PGID)

	if s.Debug {
		s.mu.Lock()
		defer s.mu.Unlock()
		f, err := os.Create(filepath.Join(s.depDir, fmt.Sprintf("%s.log", od.id)))
		if err != nil {
			return err
		}
		s.debugLog = f
		log.SetOutput(f)
	}

	return nil
}

func (s *CommandServer) EndTrace(req *TraceRequest, rep *TraceResponse) error {
	od := s.root.removeRecord(req.PGID)
	rep.ID = od.id
	rep.DepDir = s.depDir
	for _, v := range []struct {
		op   operation
		dest *[]string
	}{
		{opRead, &rep.Read},
		{opCreate, &rep.Create},
		{opUpdate, &rep.Update},
		{opDelete, &rep.Delete},
	} {
		for k := range od.ops[v.op] {
			*v.dest = append(*v.dest, k)
		}
		sort.Strings(*v.dest)
	}

	if s.Debug {
		s.mu.Lock()
		defer s.mu.Unlock()
		log.SetOutput(os.Stderr)
		s.debugLog.Close()
		s.debugLog = nil
	}
	return nil
}

func newSocket() (net.Listener, string, error) {
	dir, err := os.MkdirTemp("", "newSocket")
	if err != nil {
		return nil, "", err
	}
	s := filepath.Join(dir, "socket")
	l, err := net.Listen("unix", s)
	return l, s, err
}

// Runs a command on the server for use in the command-line program
func ClientRun(socket string, commandline string, env []string, dir string) error {
	client, err := rpc.Dial("unix", socket)
	if err != nil {
		return err
	}

	pid := os.Getpid()
	if err := syscall.Setpgid(pid, 0); err != nil {
		return err
	}

	req := TraceRequest{PGID: pid}
	var rep TraceResponse
	if err := client.Call("CommandServer.StartTrace", &req, &rep); err != nil {
		return err
	}

	cmd := exec.Command("/bin/sh", "-c", commandline)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	cmd.Dir = dir
	cmd.Env = env

	err = cmd.Run()
	err2 := client.Call("CommandServer.EndTrace", &req, &rep)
	if err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}

	fn := filepath.Join(rep.DepDir, rep.ID) + ".json"

	jsonOD := JSONOpenData{
		ID:      rep.ID,
		Command: commandline,
		Dir:     dir,
		Read:    rep.Read,
		Create:  rep.Create,
		Update:  rep.Update,
		Delete:  rep.Delete,
	}

	if data, err := json.Marshal(jsonOD); err != nil {
		return err
	} else {
		return os.WriteFile(fn, data, 0644)
	}
}
