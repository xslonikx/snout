package logging

import (
	"fmt"
	"log/syslog"
	"strings"
	"sync"
)

type Level uint8

const (
	Debug Level = iota
	Info
	Warning
	Error
)

var (
	mu      sync.Mutex
	writer  *syslog.Writer
	minimum = Info
)

func Initialize(level, facility string) error {
	if err := Validate(level, facility); err != nil {
		return err
	}
	parsedLevel, err := parseLevel(level)
	if err != nil {
		return err
	}
	parsedFacility, err := parseFacility(facility)
	if err != nil {
		return err
	}
	syslogWriter, err := syslog.New(parsedFacility|syslog.LOG_INFO, "snout")
	if err != nil {
		return fmt.Errorf("connect to syslog: %w", err)
	}

	mu.Lock()
	if writer != nil {
		_ = writer.Close()
	}
	writer = syslogWriter
	minimum = parsedLevel
	mu.Unlock()
	return nil
}

func Validate(level, facility string) error {
	if _, err := parseLevel(level); err != nil {
		return err
	}
	if _, err := parseFacility(facility); err != nil {
		return err
	}
	return nil
}

func Close() error {
	mu.Lock()
	defer mu.Unlock()
	if writer == nil {
		return nil
	}
	err := writer.Close()
	writer = nil
	return err
}

func Debugf(format string, args ...any)   { write(Debug, format, args...) }
func Infof(format string, args ...any)    { write(Info, format, args...) }
func Warningf(format string, args ...any) { write(Warning, format, args...) }
func Errorf(format string, args ...any)   { write(Error, format, args...) }

func write(level Level, format string, args ...any) {
	mu.Lock()
	defer mu.Unlock()
	if writer == nil || level < minimum {
		return
	}
	message := fmt.Sprintf(format, args...)
	switch level {
	case Debug:
		_ = writer.Debug(message)
	case Info:
		_ = writer.Info(message)
	case Warning:
		_ = writer.Warning(message)
	case Error:
		_ = writer.Err(message)
	}
}

func parseLevel(value string) (Level, error) {
	switch strings.ToLower(value) {
	case "debug":
		return Debug, nil
	case "info":
		return Info, nil
	case "warning", "warn":
		return Warning, nil
	case "error", "err":
		return Error, nil
	default:
		return 0, fmt.Errorf("unsupported log level %q", value)
	}
}

func parseFacility(value string) (syslog.Priority, error) {
	facilities := map[string]syslog.Priority{
		"daemon": syslog.LOG_DAEMON,
		"user":   syslog.LOG_USER,
		"local0": syslog.LOG_LOCAL0,
		"local1": syslog.LOG_LOCAL1,
		"local2": syslog.LOG_LOCAL2,
		"local3": syslog.LOG_LOCAL3,
		"local4": syslog.LOG_LOCAL4,
		"local5": syslog.LOG_LOCAL5,
		"local6": syslog.LOG_LOCAL6,
		"local7": syslog.LOG_LOCAL7,
	}
	facility, ok := facilities[strings.ToLower(value)]
	if !ok {
		return 0, fmt.Errorf("unsupported syslog facility %q", value)
	}
	return facility, nil
}
