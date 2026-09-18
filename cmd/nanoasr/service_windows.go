//go:build windows

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/usunrise88/nanoasr/internal/config"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

// How long the control manager and this command are each prepared to wait for
// a state change. Stopping is the slow one: the server drains its queue, and a
// job that is already decoding a long file finishes it.
const (
	serviceStopTimeout  = 90 * time.Second
	serviceStartTimeout = 30 * time.Second
)

// runAsService runs the server under the service control manager when this
// process was started by it.
//
// The check has to happen before the arguments are dispatched, because the SCM
// starts the registered command line — `nanoasr.exe serve -config ...` — and a
// process that simply serves without reporting in over the SCM pipe is killed
// after thirty seconds with "the service did not respond in a timely fashion".
func runAsService() (bool, error) {
	inService, err := svc.IsWindowsService()
	if err != nil {
		// Only reading the process token can make this fail. Guessing "service"
		// when it is a console run hangs with no output, while guessing
		// "console" when it is a service fails visibly in the SCM, so guess
		// console and say why.
		fmt.Fprintln(os.Stderr, "nanoasr: cannot tell whether this is a service:", err)
		return false, nil
	}
	if !inService {
		return false, nil
	}
	return true, svc.Run(serviceName, &windowsService{args: os.Args[1:]})
}

// windowsService adapts the server's context-cancellation shutdown to the
// control manager's state machine.
type windowsService struct{ args []string }

func (w *windowsService) Execute(_ []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	const accepted = svc.AcceptStop | svc.AcceptShutdown
	changes <- svc.Status{State: svc.StartPending}

	// A service starts in %SystemRoot%\System32 whatever the binary's location,
	// so a relative path — in the registered arguments or inside the
	// configuration — would resolve somewhere nobody meant. Moving to the
	// installation directory makes relative paths mean what they mean on a
	// console.
	if exe, err := os.Executable(); err == nil {
		_ = os.Chdir(filepath.Dir(exe))
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- serve(ctx, serveArgs(w.args)) }()

	// Reported as running as soon as the server is launched rather than once it
	// has loaded its weights: preloading gigabytes takes longer than the SCM
	// waits, and readiness is what /readyz is for.
	changes <- svc.Status{State: svc.Running, Accepts: accepted}

	for {
		select {
		case err := <-done:
			// The server returned on its own: a port already taken, a
			// configuration it will not load. A non-zero exit code is what
			// makes the restart-on-failure policy fire.
			if err != nil {
				fmt.Fprintln(os.Stderr, "nanoasr:", err)
				return false, 1
			}
			return false, 0
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				changes <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				cancel()
				return false, w.drain(done, changes)
			default:
				// An unexpected control code is not worth stopping over.
			}
		}
	}
}

// drain waits for the server to finish shutting down, telling the control
// manager that progress is still being made.
//
// Without the running checkpoint the SCM gives up after its own timeout and
// reports a service that did not stop, which is a lie whenever a long file is
// still being written out.
func (w *windowsService) drain(done <-chan error, changes chan<- svc.Status) uint32 {
	const hint = 5 * time.Second
	checkpoint := uint32(1)
	changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: uint32(hint.Milliseconds())}

	tick := time.NewTicker(hint / 2)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			if err != nil {
				fmt.Fprintln(os.Stderr, "nanoasr:", err)
				return 1
			}
			return 0
		case <-tick.C:
			checkpoint++
			changes <- svc.Status{State: svc.StopPending, CheckPoint: checkpoint, WaitHint: uint32(hint.Milliseconds())}
		}
	}
}

// serveArgs drops the leading "serve" from the registered command line, which
// is there so that the same binary path can be run by hand.
func serveArgs(args []string) []string {
	if len(args) > 0 && args[0] == "serve" {
		return args[1:]
	}
	return args
}

// serviceCommand registers, removes and controls the Windows service.
//
//	nanoasr service install [-config FILE] [-log-file FILE] [-manual] [-no-start]
//	nanoasr service uninstall
//	nanoasr service start | stop | restart | status
//
// The same verbs are accepted as `nanoasr --install` and so on.
func serviceCommand(args []string) error {
	verb, args := takePositional(args, "status")

	fs := flag.NewFlagSet("service "+verb, flag.ExitOnError)
	name := fs.String("name", serviceName, "service name in the control manager")
	cfgPath := fs.String("config", "", "install: path to nanoasr.yaml (default: the one beside the binary, or in %ProgramData%\\NanoASR)")
	logPath := fs.String("log-file", "", "install: where the service writes its log (default: logs\\nanoasr.log beside the configuration)")
	account := fs.String("account", "", `install: account to run as, e.g. ".\nanoasr" (default: LocalSystem)`)
	password := fs.String("password", "", "install: password for -account")
	manual := fs.Bool("manual", false, "install: do not start the service at boot")
	noStart := fs.Bool("no-start", false, "install: register the service but leave it stopped")
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	switch verb {
	case "install":
		return installService(*name, *cfgPath, *logPath, *account, *password, *manual, *noStart)
	case "uninstall", "remove":
		return uninstallService(*name)
	case "start":
		return startService(*name)
	case "stop":
		return stopService(*name)
	case "restart":
		if err := stopService(*name); err != nil {
			return err
		}
		return startService(*name)
	case "status":
		return statusService(*name)
	default:
		return fmt.Errorf("service %s: unknown subcommand; use %s", verb, listVerbs())
	}
}

func installService(name, cfgPath, logPath, account, password string, manual, noStart bool) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	exe, err := exePath()
	if err != nil {
		return err
	}
	cfgPath, err = resolveServiceConfig(cfgPath, filepath.Dir(exe))
	if err != nil {
		return err
	}
	if logPath == "" {
		logPath = filepath.Join(filepath.Dir(cfgPath), "logs", "nanoasr.log")
	}
	if logPath, err = filepath.Abs(logPath); err != nil {
		return err
	}
	// Created here, while the command still holds administrative rights: the
	// service account may have less, and a log directory it cannot create is a
	// service that starts and immediately exits.
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}

	startType := uint32(mgr.StartAutomatic)
	if manual {
		startType = mgr.StartManual
	}
	conf := mgr.Config{
		ServiceType:      windows.SERVICE_WIN32_OWN_PROCESS,
		StartType:        startType,
		ErrorControl:     mgr.ErrorNormal,
		DisplayName:      serviceDisplayName,
		Description:      serviceDescription,
		ServiceStartName: account,
		Password:         password,
		// Every auto-start service contends for the same disk at boot, and this
		// one reads gigabytes of weights while nothing upstream waits on it.
		DelayedAutoStart: startType == mgr.StartAutomatic,
	}
	args := []string{"serve", "-config", cfgPath, "-log-file", logPath}

	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot reach the service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	switch {
	case err == nil:
		// Installing over an existing registration is the upgrade path — a new
		// archive unpacked in a new directory, or a configuration that moved —
		// so point the service at this binary rather than refusing.
		defer s.Close()
		if err := stopAndWait(s, serviceStopTimeout); err != nil {
			return err
		}
		conf.BinaryPathName = commandLine(exe, args...)
		if err := s.UpdateConfig(conf); err != nil {
			return fmt.Errorf("cannot update the %s service: %w", name, err)
		}
	case errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST):
		s, err = m.CreateService(name, exe, conf, args...)
		if err != nil {
			return fmt.Errorf("cannot create the %s service: %w", name, err)
		}
		defer s.Close()
	default:
		return fmt.Errorf("cannot open the %s service: %w", name, err)
	}

	// Restart a failed service with a backoff, and forget the failure count
	// after a day. A crash at three in the morning should not need an operator;
	// a binary that cannot start at all should not spin.
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 15 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
	}, uint32(24*time.Hour/time.Second)); err != nil {
		fmt.Fprintf(os.Stderr, "warning: the restart-on-failure policy was not set: %v\n", err)
	}
	// A configuration the server refuses is an ordinary non-zero exit rather
	// than a crash, and the policy above ignores those unless this is set.
	if err := s.SetRecoveryActionsOnNonCrashFailures(true); err != nil {
		fmt.Fprintf(os.Stderr, "warning: restart-on-exit was not set: %v\n", err)
	}

	fmt.Printf("service     %s (%s)\n", name, serviceDisplayName)
	fmt.Printf("binary      %s\n", commandLine(exe, args...))
	fmt.Printf("config      %s\n", cfgPath)
	fmt.Printf("log         %s\n", logPath)
	fmt.Printf("start       %s\n", map[bool]string{true: "manual", false: "automatic (delayed)"}[manual])
	fmt.Printf("account     %s\n", accountNote(account))

	if noStart {
		fmt.Printf("\nStart it with:\n  %s service start\n", exe)
		return nil
	}
	if err := s.Start(); err != nil {
		return fmt.Errorf("the service is registered but did not start: %w\n"+
			"the reason is in %s", err, logPath)
	}
	st, err := waitForState(s, svc.Running, serviceStartTimeout)
	if err != nil {
		return err
	}
	fmt.Printf("state       %s\n", stateName(st.State))
	fmt.Printf("\nThe server is starting; it answers /readyz once the weights are loaded.\n"+
		"  %s service status\n  %s\n", exe, logPath)
	return nil
}

// accountNote names the account the service will run as, including the one
// Windows means by an empty string.
func accountNote(account string) string {
	if account == "" {
		return "LocalSystem"
	}
	return account
}

func uninstallService(name string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot reach the service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		// Removing what is not there is what the caller wanted, so it is not a
		// failure: an uninstall script must be safe to run twice.
		fmt.Printf("the %s service is not installed\n", name)
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot open the %s service: %w", name, err)
	}
	defer s.Close()

	if err := stopAndWait(s, serviceStopTimeout); err != nil {
		return err
	}
	if err := s.Delete(); err != nil && !errors.Is(err, windows.ERROR_SERVICE_MARKED_FOR_DELETE) {
		return fmt.Errorf("cannot remove the %s service: %w", name, err)
	}
	fmt.Printf("removed %s\n", name)
	fmt.Println("the configuration, models and job database were left in place")
	return nil
}

func startService(name string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot reach the service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return notInstalled(name, err)
	}
	defer s.Close()

	if err := s.Start(); err != nil {
		if errors.Is(err, windows.ERROR_SERVICE_ALREADY_RUNNING) {
			fmt.Printf("%s is already running\n", name)
			return nil
		}
		return fmt.Errorf("cannot start %s: %w", name, err)
	}
	st, err := waitForState(s, svc.Running, serviceStartTimeout)
	if err != nil {
		return err
	}
	fmt.Printf("%s is %s\n", name, stateName(st.State))
	return nil
}

func stopService(name string) error {
	if err := requireAdministrator(); err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("cannot reach the service control manager: %w", err)
	}
	defer m.Disconnect()

	s, err := m.OpenService(name)
	if err != nil {
		return notInstalled(name, err)
	}
	defer s.Close()

	if err := stopAndWait(s, serviceStopTimeout); err != nil {
		return err
	}
	fmt.Printf("%s is stopped\n", name)
	return nil
}

func statusService(name string) error {
	s, closeSCM, err := openForQuery(name)
	if err != nil {
		return notInstalled(name, err)
	}
	defer closeSCM()
	defer s.Close()

	st, err := s.Query()
	if err != nil {
		return err
	}
	fmt.Printf("service  %s\n", name)
	fmt.Printf("state    %s\n", stateName(st.State))
	if st.ProcessId != 0 {
		fmt.Printf("pid      %d\n", st.ProcessId)
	}
	if st.State == svc.Stopped && st.Win32ExitCode != 0 && st.Win32ExitCode != uint32(windows.ERROR_SERVICE_NEVER_STARTED) {
		fmt.Printf("exit     %d\n", st.Win32ExitCode)
	}
	// The configuration is what says which nanoasr.yaml and which log file this
	// service is actually using, which is the question behind most of the ones
	// that end up here.
	if conf, err := s.Config(); err == nil {
		fmt.Printf("start    %s\n", startTypeName(conf.StartType))
		fmt.Printf("account  %s\n", accountNote(conf.ServiceStartName))
		fmt.Printf("command  %s\n", conf.BinaryPathName)
	}
	return nil
}

// notInstalled turns the control manager's error number into the sentence an
// operator can act on.
func notInstalled(name string, err error) error {
	if errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST) {
		return fmt.Errorf("the %s service is not installed; register it with `nanoasr --install`", name)
	}
	return fmt.Errorf("cannot open the %s service: %w", name, err)
}

// stopAndWait stops a service that is running and waits for it to say so.
func stopAndWait(s *mgr.Service, timeout time.Duration) error {
	st, err := s.Query()
	if err != nil {
		return err
	}
	if st.State == svc.Stopped {
		return nil
	}
	if _, err := s.Control(svc.Stop); err != nil {
		// Already on its way down, which is what was asked for.
		if !errors.Is(err, windows.ERROR_SERVICE_NOT_ACTIVE) {
			return fmt.Errorf("cannot stop %s: %w", s.Name, err)
		}
	}
	_, err = waitForState(s, svc.Stopped, timeout)
	return err
}

// waitForState polls until the service reaches want, or until the deadline.
//
// The control manager reports a state change asynchronously, so an install that
// starts the service and then prints "running" has to ask rather than assume.
func waitForState(s *mgr.Service, want svc.State, timeout time.Duration) (svc.Status, error) {
	deadline := time.Now().Add(timeout)
	for {
		st, err := s.Query()
		if err != nil {
			return st, err
		}
		if st.State == want {
			return st, nil
		}
		if time.Now().After(deadline) {
			return st, fmt.Errorf("%s is %s after %s, not %s",
				s.Name, stateName(st.State), timeout, stateName(want))
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// openForQuery opens a service with only the rights a status query needs.
//
// Everything else here goes through mgr.Connect, which asks the control manager
// for full control and therefore requires elevation. Asking what state a
// service is in should not.
func openForQuery(name string) (*mgr.Service, func(), error) {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot reach the service control manager: %w", err)
	}
	np, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return nil, nil, err
	}
	h, err := windows.OpenService(scm, np, windows.SERVICE_QUERY_STATUS|windows.SERVICE_QUERY_CONFIG)
	if err != nil {
		windows.CloseServiceHandle(scm)
		return nil, nil, err
	}
	return &mgr.Service{Name: name, Handle: h}, func() { windows.CloseServiceHandle(scm) }, nil
}

// requireAdministrator refuses early, with the sentence that says what to do.
//
// Without it the first failure is ERROR_ACCESS_DENIED from deep inside the
// control manager, which reads like a bug in this program rather than a
// PowerShell window that was not opened as administrator.
func requireAdministrator() error {
	var admins *windows.SID
	err := windows.AllocateAndInitializeSid(
		&windows.SECURITY_NT_AUTHORITY, 2,
		windows.SECURITY_BUILTIN_DOMAIN_RID, windows.DOMAIN_ALIAS_RID_ADMINS,
		0, 0, 0, 0, 0, 0, &admins)
	if err != nil {
		return fmt.Errorf("cannot check for administrative rights: %w", err)
	}
	defer windows.FreeSid(admins)

	// The zero token is this thread's, which is what accounts for the
	// deny-only Administrators SID in an unelevated token of a split-token
	// account: membership alone is not elevation.
	member, err := windows.Token(0).IsMember(admins)
	if err != nil {
		return fmt.Errorf("cannot check for administrative rights: %w", err)
	}
	if !member {
		return errors.New("this changes the Windows service control manager and has to run elevated;\n" +
			"open PowerShell with \"Run as administrator\" and try again")
	}
	return nil
}

// resolveServiceConfig picks the configuration the service will be registered
// with and refuses one the server would not accept.
//
// Registering an unusable configuration is the most expensive mistake available
// here: the service installs, the SCM starts it, the process exits, and the
// only evidence is an event log entry saying the service terminated
// unexpectedly. The archive ships a nanoasr.yaml with no keys in it, which is
// precisely such a configuration, so this is not a theoretical case.
func resolveServiceConfig(explicit, exeDir string) (string, error) {
	path := explicit
	if path == "" {
		path = defaultServiceConfig(exeDir)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	hint := fmt.Sprintf("\nwrite one — it issues the API keys and downloads the models:\n"+
		"  nanoasr init -config %s -data-dir %s", path, filepath.Dir(path))
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("there is no configuration at %s%s", path, hint)
		}
		return "", err
	}
	if _, err := config.Load(path); err != nil {
		return "", fmt.Errorf("the configuration at %s is not one the server would start with: %w%s",
			path, err, hint+" -force")
	}
	return path, nil
}

// defaultServiceConfig is where a configuration written for a service lives.
//
// %ProgramData%\NanoASR is what the installation script uses and the only
// reason that directory would exist, so it wins over the example configuration
// that the archive unpacks beside the binary.
func defaultServiceConfig(exeDir string) string {
	if pd := os.Getenv("ProgramData"); pd != "" {
		shared := filepath.Join(pd, "NanoASR", "nanoasr.yaml")
		if _, err := os.Stat(shared); err == nil {
			return shared
		}
	}
	return filepath.Join(exeDir, "nanoasr.yaml")
}

// exePath is this binary, resolved: the control manager stores the command line
// verbatim, so a relative path or a symlink would be recorded as one.
func exePath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return filepath.Abs(exe)
}

// commandLine quotes a binary and its arguments the way CreateService does, so
// that an update writes the same string a fresh install would.
func commandLine(exe string, args ...string) string {
	s := syscall.EscapeArg(exe)
	for _, a := range args {
		s += " " + syscall.EscapeArg(a)
	}
	return s
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending:
		return "resuming"
	case svc.PausePending:
		return "pausing"
	case svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}

func startTypeName(t uint32) string {
	switch t {
	case mgr.StartAutomatic:
		return "automatic"
	case mgr.StartManual:
		return "manual"
	case mgr.StartDisabled:
		return "disabled"
	}
	return fmt.Sprintf("type %d", t)
}
