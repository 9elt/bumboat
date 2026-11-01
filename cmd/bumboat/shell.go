package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/9elt/bumboat/api"
	"github.com/charmbracelet/x/term"
	"github.com/muesli/cancelreader"
)

// shell is a tea.ExecCommand: the TUI hands it the terminal and gets it back on exit.
type shell struct {
	c       api.BumboatClient
	service string
	in      io.Reader
	out     io.Writer
}

func (s *shell) SetStdin(r io.Reader)  { s.in = r }
func (s *shell) SetStdout(w io.Writer) { s.out = w }
func (s *shell) SetStderr(io.Writer)   {}

func (s *shell) Run() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := s.c.Shell(ctx)
	if err != nil {
		return err
	}
	var mu sync.Mutex
	send := func(in *api.ShellInput) error {
		mu.Lock()
		defer mu.Unlock()
		return stream.Send(in)
	}
	fd := os.Stdin.Fd()
	if f, ok := s.in.(*os.File); ok {
		fd = f.Fd()
	}
	size := func() *api.ShellInput {
		w, h, _ := term.GetSize(fd)
		return &api.ShellInput{Rows: uint32(h), Cols: uint32(w)}
	}
	first := size()
	first.Service, first.Term = s.service, os.Getenv("TERM")
	if err := send(first); err != nil {
		return err
	}
	fmt.Fprintf(s.out, "\x1b[H\x1b[2Jshell in %s, exit to go back\r\n", s.service)
	if state, err := term.MakeRaw(fd); err == nil {
		defer term.Restore(fd, state)
	}

	// a cancelable reader, so no keystroke is lost to it once the TUI is back
	in, err := cancelreader.NewReader(s.in)
	if err != nil {
		return err
	}
	defer in.Cancel()
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if err != nil || send(&api.ShellInput{Data: buf[:n]}) != nil {
				return
			}
		}
	}()
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)
	go func() {
		for {
			select {
			case <-winch:
				send(size())
			case <-ctx.Done():
				return
			}
		}
	}()

	for {
		res, err := stream.Recv()
		if err != nil {
			return err
		}
		if res.Exited && (res.ExitCode == 126 || res.ExitCode == 127) {
			return fmt.Errorf("no shell in %s's image", s.service)
		} else if res.Exited {
			return nil
		}
		s.out.Write(res.Data)
	}
}
