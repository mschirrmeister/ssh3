package main

import (
	// "bufio"
	// "context"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	_ "net/http/pprof"

	"github.com/creack/pty"
	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	// "github.com/quic-go/quic-go/logging"
	// "github.com/quic-go/quic-go/qlog"

	testdata "ssh3"
	ssh3 "ssh3/src"
	"ssh3/src/auth"
	ssh3Messages "ssh3/src/message"
	util "ssh3/src/util"
)

var signals = map[string]os.Signal {
	"SIGABRT":    syscall.Signal(0x6),
	"SIGALRM":    syscall.Signal(0xe),
	"SIGBUS":     syscall.Signal(0x7),
	"SIGCHLD":    syscall.Signal(0x11),
	"SIGCLD":     syscall.Signal(0x11),
	"SIGCONT":    syscall.Signal(0x12),
	"SIGFPE":     syscall.Signal(0x8),
	"SIGHUP":     syscall.Signal(0x1),
	"SIGILL":     syscall.Signal(0x4),
	"SIGINT":     syscall.Signal(0x2),
	"SIGIO":      syscall.Signal(0x1d),
	"SIGIOT":     syscall.Signal(0x6),
	"SIGKILL":    syscall.Signal(0x9),
	"SIGPIPE":    syscall.Signal(0xd),
	"SIGPOLL":    syscall.Signal(0x1d),
	"SIGPROF":    syscall.Signal(0x1b),
	"SIGPWR":     syscall.Signal(0x1e),
	"SIGQUIT":    syscall.Signal(0x3),
	"SIGSEGV":    syscall.Signal(0xb),
	"SIGSTKFLT":  syscall.Signal(0x10),
	"SIGSTOP":    syscall.Signal(0x13),
	"SIGSYS":     syscall.Signal(0x1f),
	"SIGTERM":    syscall.Signal(0xf),
	"SIGTRAP":    syscall.Signal(0x5),
	"SIGTSTP":    syscall.Signal(0x14),
	"SIGTTIN":    syscall.Signal(0x15),
	"SIGTTOU":    syscall.Signal(0x16),
	"SIGUNUSED":  syscall.Signal(0x1f),
	"SIGURG":     syscall.Signal(0x17),
	"SIGUSR1":    syscall.Signal(0xa),
	"SIGUSR2":    syscall.Signal(0xc),
	"SIGVTALRM":  syscall.Signal(0x1a),
	"SIGWINCH":   syscall.Signal(0x1c),
	"SIGXCPU":    syscall.Signal(0x18),
	"SIGXFSZ":    syscall.Signal(0x19),
}

type channelType uint64

const ( 
	LARVAL = channelType(iota)
	OPEN
	

)

type openPty struct {
	pty *os.File	// pty used by the server/user to communicate with the running process
	tty *os.File	// tty used by the running process to communicate with the server/user
	winSize *pty.Winsize
	term string
}

type runningCommand struct {
	exec.Cmd
	stdoutR io.Reader
	stderrR io.Reader
	stdinW  io.Writer
}

type runningSession struct {
	channelState channelType
	pty *openPty
	runningCmd *runningCommand
}

var conversations = make(map[quic.Stream]*ssh3.Conversation)
var runningSessions = make(map[*ssh3.Channel]*runningSession)

func setWinsize(f *os.File, charWidth, charHeight, pixWidth, pixHeight uint64) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(charHeight), uint16(charWidth), uint16(pixWidth), uint16(pixHeight)})))
}

type binds []string

func (b binds) String() string {
	return strings.Join(b, ",")
}

func (b *binds) Set(v string) error {
	*b = strings.Split(v, ",")
	return nil
}

// Size is needed by the /demo/upload handler to determine the size of the uploaded file
type Size interface {
	Size() int64
}

// See https://en.wikipedia.org/wiki/Lehmer_random_number_generator
func generatePRData(l int) []byte {
	res := make([]byte, l)
	seed := uint64(1)
	for i := 0; i < l; i++ {
		seed = seed * 48271 % 2147483647
		res[i] = byte(seed)
	}
	return res
}

func execCmdInBackground(channel *ssh3.Channel, openPty *openPty, runningCommand *runningCommand) error {
	// TODO: set the environment like in do_setup_env of https://github.com/openssh/openssh-portable/blob/master/session.c	
	if openPty != nil {
		err := util.StartWithSizeAndPty(&runningCommand.Cmd, openPty.winSize, openPty.pty, openPty.tty)
		if err != nil {
			return err
		}
	} else {
		err := runningCommand.Start()
		if err != nil {
			return err
		}
	}
	
	go func() {

		type readResult struct {
			data []byte
			err error
		}
	
		stdoutChan := make(chan readResult, 1)
		stderrChan := make(chan readResult, 1)
	
		readStdout := func() {
			if runningCommand.stdoutR != nil {
				for {
					buf := make([]byte, channel.MaxPacketSize)
					n, err := runningCommand.stdoutR.Read(buf)
					out := make([]byte, n)
					copy(out, buf[:n])
					stdoutChan <- readResult{ data: out, err: err }
					if err != nil {
						return
					}
				}
			}
		}
		readStderr := func() {
			if runningCommand.stderrR != nil {
				for {
					buf := make([]byte, channel.MaxPacketSize)
					n, err := runningCommand.stderrR.Read(buf)
					out := make([]byte, n)
					copy(out, buf[:n])
					stderrChan <- readResult{ data: out, err: err }
					if err != nil {
						return
					}
				}
			}
		}
	
		go readStdout()
		go readStderr()
	
		defer func() {
			err := runningCommand.Wait()
			exitstatus := uint64(0)
			if err != nil {
				if exitError, ok := err.(*exec.ExitError); ok {
					exitstatus = uint64(exitError.ExitCode())
				}
			}
			fmt.Println("DEBUG: exited with status", exitstatus)
			channel.SendRequest(&ssh3Messages.ChannelRequestMessage{
				WantReply: false,
				ChannelRequest: &ssh3Messages.ExitStatusRequest{ ExitStatus: exitstatus },
			})
		}()
		for {
			select {
			case stdoutResult := <-stdoutChan:
				buf, err := stdoutResult.data, stdoutResult.err
				_, err2 := channel.WriteData(buf, ssh3Messages.SSH_EXTENDED_DATA_NONE)
				if err2 != nil {
					fmt.Fprintf(os.Stderr, "could not write the pty's output in an SSH message: %+v\n", err)
					return
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "could not read the pty's output: %+v\n", err)
					return
				}
			
			case stderrResult := <-stderrChan:
				buf, err := stderrResult.data, stderrResult.err
				_, err2 := channel.WriteData(buf, ssh3Messages.SSH_EXTENDED_DATA_STDERR)
				if err2 != nil {
					fmt.Fprintf(os.Stderr, "could not write the pty's output in an SSH message: %+v\n", err)
					return
				}
				if err != nil {
					fmt.Fprintf(os.Stderr, "could not read the pty's output: %+v\n", err)
					return
				}
			}
		}
	}()
	return nil
}

func newPtyReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.PtyRequest, wantReply bool) error {
	var session *runningSession
	session, ok := runningSessions[channel]
	if !ok {
		return fmt.Errorf("internal error: cannot find session for current channel")
	}

	if session.channelState != LARVAL {
		return fmt.Errorf("cannot request new pty on already established session")
	}

	if session.pty != nil {
		return fmt.Errorf("cannot request new pty on a channel with an already existing pty")
	}
	winSize := &pty.Winsize{Rows: uint16(request.CharHeight), Cols: uint16(request.CharWidth), X: uint16(request.PixelWidth), Y: uint16(request.PixelHeight)}
	fmt.Println("PTY REQUEST", request, channel.MaxPacketSize)
	pty, tty, err := pty.Open()
	if err != nil {
		return err
	}

	setWinsize(pty, request.CharWidth, request.CharHeight, request.PixelWidth, request.PixelHeight)

	session.pty = &openPty{
		pty: pty,
		tty: tty,
		term: request.Term,
		winSize: winSize,
	}

	return nil
}

func newX11Req(user *auth.User, channel *ssh3.Channel, request ssh3Messages.X11Request, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newShellReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.ShellRequest, wantReply bool) error {
	var session *runningSession
	session, ok := runningSessions[channel]
	if !ok {
		return fmt.Errorf("internal error: cannot find session for current channel")
	}

	if session.channelState != LARVAL {
		return fmt.Errorf("cannot request new shell on already established session")
	}

	env := ""
	if session.pty != nil {
		env = fmt.Sprintf("TERM=%s", session.pty.term)
	}

	var stdoutR, stderrR, stdinR io.Reader
	var stdoutW, stderrW, stdinW io.Writer
	var err error = nil

	if session.pty != nil {
		stdoutW = session.pty.tty
		stderrW = session.pty.tty
		stdinR  = session.pty.tty

		stdoutR = session.pty.pty
		stderrR = nil
		stdinW =  session.pty.pty
	} else {
		stdoutR, stdoutW, err = os.Pipe()
		if err != nil {
			return err
		}
		stderrR, stderrW, err = os.Pipe()
		if err != nil {
			return err
		}
		stdinR, stdinW, err = os.Pipe()
		if err != nil {
			return err
		}
	}

	cmd := user.CreateShellCommand(env, stdoutW, stderrW, stdinR)

	runningCommand := &runningCommand{
		Cmd: *cmd,
		stdoutR: stdoutR,
		stderrR: stderrR,
		stdinW: stdinW,
	}

	session.runningCmd = runningCommand

	session.channelState = OPEN

	return execCmdInBackground(channel, session.pty, session.runningCmd)
}

func newExecReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.ExecRequest, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newSubsystemReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.SubsystemRequest, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newWindowChangeReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.WindowChangeRequest, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newSignalReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.SignalRequest, wantReply bool) error {
	runningSession, ok := runningSessions[channel]
	if !ok {
		return fmt.Errorf("could not find running session for channel %d (conv %d)", channel.ChannelID, channel.ConversationID)
	}

	if runningSession.channelState == LARVAL {
		return fmt.Errorf("cannot send signal for channel in LARVAL state (channel %d, conv %d)", channel.ChannelID, channel.ConversationID)
	}

	switch channel.ChannelType {
	case "session":
		if runningSession.runningCmd == nil {
			return fmt.Errorf("there is no running command on Channel %d (conv %d) to feed the received data", channel.ChannelID, channel.ConversationID)
		}
		signal, ok := signals["SIG" + request.SignalNameWithoutSig]
		if !ok {
			return fmt.Errorf("unhandled signal SIG%s", request.SignalNameWithoutSig)
		}
		runningSession.runningCmd.Process.Signal(signal)
	default:
		return fmt.Errorf("channel type %s not implemented", channel.ChannelType)
	}
	return nil
}

func newExitStatusReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.ExitStatusRequest, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newExitSignalReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.ExitSignalRequest, wantReply bool) error {
	return fmt.Errorf("%T not implemented", request)
}

func newDataReq(user *auth.User, channel *ssh3.Channel, request ssh3Messages.DataOrExtendedDataMessage) error {
	runningSession, ok := runningSessions[channel]
	if !ok {
		return fmt.Errorf("could not find running session for channel %d (conv %d)", channel.ChannelID, channel.ConversationID)
	}

	if runningSession.channelState == LARVAL {
		return fmt.Errorf("cannot receive data for channel in LARVAL state (channel %d, conv %d)", channel.ChannelID, channel.ConversationID)
	}

	

	switch channel.ChannelType {
	case "session":
		fmt.Println("handle new data req")
		if runningSession.runningCmd == nil {
			return fmt.Errorf("there is no running command on Channel %d (conv %d) to feed the received data", channel.ChannelID, channel.ConversationID)
		}
		switch request.DataType {
		case ssh3Messages.SSH_EXTENDED_DATA_NONE:
			runningSession.runningCmd.stdinW.Write([]byte(request.Data))
		default:
			return fmt.Errorf("extended data type forbidden server PTY")
		}
	default:
		return fmt.Errorf("channel type %s not implemented", channel.ChannelType)
	}
	return nil
}

func main() {
	bs := binds{}
	flag.Var(&bs, "bind", "bind to")
	flag.Parse()


	if len(bs) == 0 {
		bs = binds{"localhost:6121"}
	}

	quicConf := &quic.Config{}
	// if *enableQlog {
	// 	quicConf.Tracer = func(ctx context.Context, p logging.Perspective, connID quic.ConnectionID) logging.ConnectionTracer {
	// 		filename := fmt.Sprintf("server_%x.qlog", connID)
	// 		f, err := os.Create(filename)
	// 		if err != nil {
	// 			log.Fatal(err)
	// 		}
	// 		log.Printf("Creating qlog file %s.\n", filename)
	// 		return qlog.NewConnectionTracer(utils.NewBufferedWriteCloser(bufio.NewWriter(f), f), p, connID)
	// 	}
	// }


	var wg sync.WaitGroup
	wg.Add(len(bs))
	for _, b := range bs {
		bCap := b
		go func() {
			var err error

			server := http3.Server{
				Handler:    nil,
				Addr:       bCap,
				QuicConfig: quicConf,
			}
			certFile, keyFile := testdata.GetCertificatePaths()

			mux := http.NewServeMux()
			ssh3Server := ssh3.NewServer(30000, &server, func(authenticatedUsername string, conv *ssh3.Conversation) error {
				authenticatedUser, err := auth.GetUser(authenticatedUsername)
				if err != nil {
					return err
				}
				for {
					channel, err := conv.AcceptChannel(context.Background())
					if err != nil {
						return err
					}
					runningSessions[channel] = &runningSession{
						channelState: LARVAL,
						pty: nil,
						runningCmd: nil,
					}
					go func() {
						defer channel.Close()
						for {
							genericMessage, err := channel.NextMessage()
							if err != nil {
								fmt.Printf("error when getting message: %+v", err)
								return
							}
							switch message := genericMessage.(type) {
								case *ssh3Messages.ChannelRequestMessage:
									switch requestMessage := message.ChannelRequest.(type) {
										case *ssh3Messages.PtyRequest:
											err = newPtyReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.X11Request:
											err = newX11Req(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.ShellRequest:
											err = newShellReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.ExecRequest:
											err = newExecReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.SubsystemRequest:
											err = newSubsystemReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.WindowChangeRequest:
											err = newWindowChangeReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.SignalRequest:
											err = newSignalReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.ExitStatusRequest:
											err = newExitStatusReq(authenticatedUser, channel, *requestMessage, message.WantReply)
										case *ssh3Messages.ExitSignalRequest:
											err = newExitSignalReq(authenticatedUser, channel, *requestMessage, message.WantReply)
									}
								case *ssh3Messages.DataOrExtendedDataMessage:
									err = newDataReq(authenticatedUser, channel, *message)
							}
							if err != nil {
								fmt.Fprintf(os.Stderr, "error while processing message: %+v: %+v\n", genericMessage, err)
								return
							}
						}
					}()
				}
			})
			ssh3Handler := ssh3Server.GetHTTPHandlerFunc()
			mux.HandleFunc("/ssh3-pty", auth.HandleBasicAuth(ssh3Handler))
			server.Handler = mux
			err = server.ListenAndServeTLS(certFile, keyFile)
			
			if err != nil {
				fmt.Println(err)
			}
			wg.Done()
		}()
	}
	wg.Wait()
}