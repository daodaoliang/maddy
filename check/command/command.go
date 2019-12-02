package command

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"github.com/emersion/go-message/textproto"
	"github.com/foxcpp/maddy/buffer"
	"github.com/foxcpp/maddy/check"
	"github.com/foxcpp/maddy/config"
	"github.com/foxcpp/maddy/exterrors"
	"github.com/foxcpp/maddy/log"
	"github.com/foxcpp/maddy/module"
	"github.com/foxcpp/maddy/target"
)

const modName = "command"

type Stage string

const (
	StageConnection = "conn"
	StageSender     = "sender"
	StageRcpt       = "rcpt"
	StageBody       = "body"
)

var placeholderRe = regexp.MustCompile(`{[a-zA-Z0-9_]+?}`)

type Check struct {
	instName string
	log      log.Logger

	stage   Stage
	actions map[int]check.FailAction
	cmd     string
	cmdArgs []string
}

func New(modName, instName string, aliases, inlineArgs []string) (module.Module, error) {
	c := &Check{
		instName: instName,
		actions: map[int]check.FailAction{
			1: check.FailAction{
				Reject: true,
			},
			2: check.FailAction{
				Quarantine: true,
			},
		},
	}

	if len(inlineArgs) == 0 {
		return nil, errors.New("command: at least one argument is required (command name)")
	}

	c.cmd = inlineArgs[0]
	c.cmdArgs = inlineArgs[1:]

	return c, nil
}

func (c *Check) Name() string {
	return modName
}

func (c *Check) InstanceName() string {
	return c.instName
}

func (c *Check) Init(cfg *config.Map) error {
	// Check whether the inline argument command is usable.
	if _, err := exec.LookPath(c.cmd); err != nil {
		return fmt.Errorf("command: %w", err)
	}

	cfg.Enum("run_on", false, false,
		[]string{StageConnection, StageSender, StageRcpt, StageBody}, StageBody,
		(*string)(&c.stage))

	cfg.AllowUnknown()
	unknown, err := cfg.Process()
	if err != nil {
		return err
	}

	for _, node := range unknown {
		switch node.Name {
		case "code":
			if len(node.Args) < 2 {
				return config.NodeErr(&node, "at least two arguments are required: <code> <action>")
			}
			exitCode, err := strconv.Atoi(node.Args[0])
			if err != nil {
				return config.NodeErr(&node, "%v", err)
			}
			action, err := check.ParseActionDirective(node.Args[1:])
			if err != nil {
				return config.NodeErr(&node, "%v", err)
			}

			c.actions[exitCode] = action
		default:
			return config.NodeErr(&node, "unexpected directive: %v", node.Name)
		}
	}

	return nil
}

type state struct {
	c       *Check
	msgMeta *module.MsgMetadata
	log     log.Logger

	mailFrom string
	rcpts    []string
}

func (c *Check) CheckStateForMsg(msgMeta *module.MsgMetadata) (module.CheckState, error) {
	return &state{
		c:       c,
		msgMeta: msgMeta,
		log:     target.DeliveryLogger(c.log, msgMeta),
	}, nil
}

func (s *state) expandCommand(address string) (string, []string) {
	expArgs := make([]string, len(s.c.cmdArgs))

	for i, arg := range s.c.cmdArgs {
		expArgs[i] = placeholderRe.ReplaceAllStringFunc(arg, func(placeholder string) string {
			switch placeholder {
			case "{auth_user}":
				if s.msgMeta.Conn == nil {
					return ""
				}
				return s.msgMeta.Conn.AuthUser
			case "{source_ip}":
				if s.msgMeta.Conn == nil {
					return ""
				}
				tcpAddr, _ := s.msgMeta.Conn.RemoteAddr.(*net.TCPAddr)
				if tcpAddr == nil {
					return ""
				}
				return tcpAddr.IP.String()
			case "{source_host}":
				if s.msgMeta.Conn == nil {
					return ""
				}
				return s.msgMeta.Conn.Hostname
			case "{source_rdns}":
				if s.msgMeta.Conn == nil {
					return ""
				}
				val, _ := s.msgMeta.Conn.RDNSName.Get().(string)
				if val == "" {
					return ""
				}
				return ""
			case "{msg_id}":
				return s.msgMeta.ID
			case "{sender}":
				return s.mailFrom
			case "{rcpts}":
				return strings.Join(s.rcpts, "\n")
			case "{address}":
				return address
			}
			return placeholder
		})
	}

	return s.c.cmd, expArgs
}

func (s *state) run(cmdName string, args []string, stdin io.Reader) module.CheckResult {
	cmd := exec.Command(cmdName, args...)
	cmd.Stdin = stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:      450,
				Message:   "Internal server error",
				CheckName: "command",
				Err:       err,
				Misc: map[string]interface{}{
					"cmd": cmd.String(),
				},
			},
			Reject: true,
		}
	}

	if err := cmd.Start(); err != nil {
		return module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:      450,
				Message:   "Internal server error",
				CheckName: "command",
				Err:       err,
				Misc: map[string]interface{}{
					"cmd": cmd.String(),
				},
			},
			Reject: true,
		}
	}
	defer cmd.Process.Signal(os.Interrupt)

	bufOut := bufio.NewReader(stdout)
	hdr, err := textproto.ReadHeader(bufOut)
	if err != nil && !errors.Is(err, io.EOF) {
		return module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:      450,
				Message:   "Internal server error",
				CheckName: "command",
				Err:       err,
				Misc: map[string]interface{}{
					"cmd": cmd.String(),
				},
			},
			Reject: true,
		}
	}

	res := module.CheckResult{}
	res.Header = hdr

	err = cmd.Wait()
	if err != nil {
		return s.errorRes(err, res, cmd.String())

	}
	return res
}

func (s *state) errorRes(err error, res module.CheckResult, cmdLine string) module.CheckResult {
	exitErr, ok := err.(*exec.ExitError)
	if !ok {
		res.Reason = &exterrors.SMTPError{
			Code:      450,
			Message:   "Internal server error",
			CheckName: "command",
			Err:       err,
			Misc: map[string]interface{}{
				"cmd": cmdLine,
			},
		}
		res.Reject = true
		return res
	}

	action, ok := s.c.actions[exitErr.ExitCode()]
	if !ok {
		res.Reason = &exterrors.SMTPError{
			Code:      450,
			Message:   "Internal server error",
			CheckName: "command",
			Err:       err,
			Reason:    "unexpected exit code",
			Misc: map[string]interface{}{
				"cmd":       cmdLine,
				"exit_code": exitErr.ExitCode(),
			},
		}
		res.Reject = true
		return res
	}

	res.Reason = &exterrors.SMTPError{
		Code:         550,
		EnhancedCode: exterrors.EnhancedCode{5, 7, 1},
		Message:      "Message rejected for due to a local policy",
		CheckName:    "command",
		Misc: map[string]interface{}{
			"cmd":       cmdLine,
			"exit_code": exitErr.ExitCode(),
		},
	}

	return action.Apply(res)
}

func (s *state) CheckConnection() module.CheckResult {
	if s.c.stage != StageConnection {
		return module.CheckResult{}
	}

	cmdName, cmdArgs := s.expandCommand("")
	return s.run(cmdName, cmdArgs, bytes.NewReader(nil))
}

func (s *state) CheckSender(addr string) module.CheckResult {
	s.mailFrom = addr

	if s.c.stage != StageSender {
		return module.CheckResult{}
	}

	cmdName, cmdArgs := s.expandCommand(addr)
	return s.run(cmdName, cmdArgs, bytes.NewReader(nil))
}

func (s *state) CheckRcpt(addr string) module.CheckResult {
	s.rcpts = append(s.rcpts, addr)

	if s.c.stage != StageRcpt {
		return module.CheckResult{}
	}

	cmdName, cmdArgs := s.expandCommand(addr)
	return s.run(cmdName, cmdArgs, bytes.NewReader(nil))
}

func (s *state) CheckBody(hdr textproto.Header, body buffer.Buffer) module.CheckResult {
	if s.c.stage != StageBody {
		return module.CheckResult{}
	}

	cmdName, cmdArgs := s.expandCommand("")

	var buf bytes.Buffer
	_ = textproto.WriteHeader(&buf, hdr)
	bR, err := body.Open()
	if err != nil {
		return module.CheckResult{
			Reason: &exterrors.SMTPError{
				Code:      450,
				Message:   "Internal server error",
				CheckName: "command",
				Err:       err,
				Misc: map[string]interface{}{
					"cmd": cmdName + " " + strings.Join(cmdArgs, " "),
				},
			},
			Reject: true,
		}
	}

	return s.run(cmdName, cmdArgs, io.MultiReader(bytes.NewReader(buf.Bytes()), bR))
}

func (s *state) Close() error {
	return nil
}

func init() {
	module.Register(modName, New)
}
