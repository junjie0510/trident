// Copyright 2018 NetApp, Inc. All Rights Reserved.

package logging

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/ssh/terminal"

	"github.com/netapp/trident/config"
	"github.com/netapp/trident/utils"
)

// InitLogging configures logging for nDVP.  Logs are written both to a log file as well as stdout/stderr.
// Since logrus doesn't support multiple writers, each log stream is implemented as a hook.
func InitLogging(logName string) error {

	// No output except for the hooks
	log.SetOutput(ioutil.Discard)

	// Write to the log file
	logFileHook, err := NewFileHook(logName)
	if err != nil {
		return fmt.Errorf("could not initialize logging to file %s: %v", logFileHook.GetLocation(), err)
	}
	log.AddHook(logFileHook)

	// Write to stdout/stderr
	log.AddHook(NewConsoleHook())

	// Remind users where the log file lives
	log.WithFields(log.Fields{
		"logLevel":        log.GetLevel().String(),
		"logFileLocation": logFileHook.GetLocation(),
		"buildTime":       config.BuildTime,
	}).Info("Initialized logging.")

	customInterval := os.Getenv(RandomLogcheckEnvVar)
	if customInterval != "" {
		customIntervalValue, err := strconv.Atoi(customInterval)
		if err == nil {
			randomLogcheckInterval = customIntervalValue
		}
	}

	return nil
}

// InitLogLevel configures the logging level.  The debug flag takes precedence if set,
// otherwise the logLevel flag (debug, info, warn, error, fatal) is used.
func InitLogLevel(debug bool, logLevel string) error {
	if debug {
		log.SetLevel(log.DebugLevel)
	} else {
		level, err := log.ParseLevel(logLevel)
		if err != nil {
			return err
		}
		log.SetLevel(level)
	}
	return nil
}

// ConsoleHook sends log entries to stdout.
type ConsoleHook struct {
	formatter log.Formatter
}

// NewConsoleHook creates a new log hook for writing to stdout/stderr.
func NewConsoleHook() *ConsoleHook {

	formatter := &log.TextFormatter{FullTimestamp: true}
	return &ConsoleHook{formatter}
}

func (hook *ConsoleHook) Levels() []log.Level {
	return log.AllLevels
}

func (hook *ConsoleHook) checkIfTerminal(w io.Writer) bool {
	switch v := w.(type) {
	case *os.File:
		return terminal.IsTerminal(int(v.Fd()))
	default:
		return false
	}
}

func (hook *ConsoleHook) Fire(entry *log.Entry) error {

	// Determine output stream
	var logWriter io.Writer
	switch entry.Level {
	case log.DebugLevel, log.InfoLevel, log.WarnLevel:
		logWriter = os.Stdout
	case log.ErrorLevel, log.FatalLevel, log.PanicLevel:
		logWriter = os.Stderr
	}

	// Write log entry to output stream
	hook.formatter.(*log.TextFormatter).ForceColors = hook.checkIfTerminal(logWriter)
	lineBytes, err := hook.formatter.Format(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to read entry, %v", err)
		return err
	}
	if len(lineBytes) > MaxLogEntryLength {
		logWriter.Write(lineBytes[:MaxLogEntryLength])
		logWriter.Write([]byte("<truncated>\n"))
	} else {
		logWriter.Write(lineBytes)
	}

	return nil
}

// FileHook sends log entries to a file.
type FileHook struct {
	logFileLocation string
	formatter       log.Formatter
	mutex           *sync.Mutex
}

// NewFileHook creates a new log hook for writing to a file.
func NewFileHook(logName string) (*FileHook, error) {

	formatter := &PlainTextFormatter{}

	// If config.LogRoot doesn't exist, make it
	dir, err := os.Lstat(LogRoot)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(LogRoot, 0755); err != nil {
			return nil, fmt.Errorf("could not create log directory %v. %v", LogRoot, err)
		}
	}
	// If config.LogRoot isn't a directory, return an error
	if dir != nil && !dir.IsDir() {
		return nil, fmt.Errorf("log path %v exists and is not a directory, please remove it", LogRoot)
	}

	// Build log file path
	logFileLocation := ""
	switch runtime.GOOS {
	case utils.Linux:
		logFileLocation = LogRoot + "/" + logName + ".log"
		break
	case utils.Darwin:
		logFileLocation = LogRoot + "/" + logName + ".log"
		break
	case utils.Windows:
		logFileLocation = logName + ".log"
		break
	}

	return &FileHook{logFileLocation, formatter, &sync.Mutex{}}, nil
}

func (hook *FileHook) Levels() []log.Level {
	return log.AllLevels
}

func (hook *FileHook) Fire(entry *log.Entry) error {

	// Get formatted entry
	lineBytes, err := hook.formatter.Format(entry)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not read log entry. %v", err)
		return err
	}

	// Write log entry to file
	logFile, err := hook.openFile()
	if err != nil {
		return err
	}
	logFile.WriteString(string(lineBytes))
	logFile.Close()

	// Rotate the file as needed
	hook.maybeDoLogfileRotation()

	return nil
}

func (hook *FileHook) GetLocation() string {
	return hook.logFileLocation
}

func (hook *FileHook) openFile() (*os.File, error) {

	logFile, err := os.OpenFile(hook.logFileLocation, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0666)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Could not open log file %v. %v", hook.logFileLocation, err)
		return nil, err
	}
	return logFile, nil
}

// logfileNeedsRotation checks to see if a file has grown too large
func (hook *FileHook) logfileNeedsRotation() bool {
	logFile, err := hook.openFile()
	if err != nil {
		return false
	}

	fileInfo, err := logFile.Stat()
	if err != nil {
		logFile.Close()
		return false
	}

	size := fileInfo.Size()
	logFile.Close()

	if size >= LogRotationThreshold {
		return true
	}

	return false
}

// maybeDoLogfileRotation prevents descending into doLogfileRotation on every call as the inner
// func is somewhat expensive and doesn't really need to happen every log entry.
func (hook *FileHook) maybeDoLogfileRotation() error {
	// Could use a counter or some other heuristic to decide when to do this, but it's
	// more a less a wash to let rand() do it every 1/n times.
	if rand.Intn(randomLogcheckInterval) == 0 {
		return hook.doLogfileRotation()
	}
	return nil
}

func (hook *FileHook) doLogfileRotation() error {
	// We use a mutex to protect rotation from concurrent loggers, but in order to avoid
	// contention over this resource with high logging levels, check the file before taking
	// the lock.  Only if the file needs rotating do we then acquire the lock and recheck
	// the size under it.  The winner of the lock race will rotate the file.
	if hook.logfileNeedsRotation() {
		hook.mutex.Lock()
		defer hook.mutex.Unlock()

		if hook.logfileNeedsRotation() {
			// Do the rotation.  The Rename call will overwrite any previous .old file.
			oldLogFileLocation := hook.logFileLocation + ".old"
			os.Rename(hook.logFileLocation, oldLogFileLocation)
		}
	}

	return nil
}

// PlainTextFormatter is a formatter than does no coloring *and* does not insist on writing logs as key/value pairs.
type PlainTextFormatter struct {

	// TimestampFormat to use for display when a full timestamp is printed
	TimestampFormat string

	// The fields are sorted by default for a consistent output. For applications
	// that log extremely frequently and don't use the JSON formatter this may not
	// be desired.
	DisableSorting bool
}

func (f *PlainTextFormatter) Format(entry *log.Entry) ([]byte, error) {

	var b *bytes.Buffer
	var keys = make([]string, 0, len(entry.Data))
	for k := range entry.Data {
		keys = append(keys, k)
	}

	if !f.DisableSorting {
		sort.Strings(keys)
	}
	if entry.Buffer != nil {
		b = entry.Buffer
	} else {
		b = &bytes.Buffer{}
	}

	f.prefixFieldClashes(entry.Data)

	timestampFormat := f.TimestampFormat
	if timestampFormat == "" {
		timestampFormat = time.RFC3339
	}
	f.printUncolored(b, entry, keys, timestampFormat)
	b.WriteByte('\n')

	return b.Bytes(), nil
}

func (f *PlainTextFormatter) prefixFieldClashes(data log.Fields) {
	if t, ok := data["time"]; ok {
		data["fields.time"] = t
	}

	if m, ok := data["msg"]; ok {
		data["fields.msg"] = m
	}

	if l, ok := data["level"]; ok {
		data["fields.level"] = l
	}
}

func (f *PlainTextFormatter) printUncolored(b *bytes.Buffer, entry *log.Entry, keys []string, timestampFormat string) {

	levelText := strings.ToUpper(entry.Level.String())[0:4]

	fmt.Fprintf(b, "%s[%s] %-44s ", levelText, entry.Time.Format(timestampFormat), entry.Message)
	for _, k := range keys {
		v := entry.Data[k]
		fmt.Fprintf(b, " %s=", k)
		f.appendValue(b, v)
	}
}

func (f *PlainTextFormatter) needsQuoting(text string) bool {
	for _, ch := range text {
		if !((ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '-' || ch == '.') {
			return true
		}
	}
	return false
}

func (f *PlainTextFormatter) appendValue(b *bytes.Buffer, value interface{}) {
	switch value := value.(type) {
	case string:
		if !f.needsQuoting(value) {
			b.WriteString(value)
		} else {
			fmt.Fprintf(b, "%q", value)
		}
	case error:
		errmsg := value.Error()
		if !f.needsQuoting(errmsg) {
			b.WriteString(errmsg)
		} else {
			fmt.Fprintf(b, "%q", errmsg)
		}
	default:
		fmt.Fprint(b, value)
	}
}
