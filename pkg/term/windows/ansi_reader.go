// +build windows

package windows

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"unsafe"

	. "github.com/Azure/go-ansiterm"
	. "github.com/Azure/go-ansiterm/winterm"
)

// ansiReader wraps a standard input file (e.g., os.Stdin) providing ANSI sequence translation.
type ansiReader struct {
	file     *os.File
	fd       uintptr
	buffer   []byte
	cbBuffer int
	command  []byte
	// TODO(azlinux): Remove this and hard-code the string -- it is not going to change
	escapeSequence []byte
}

func newAnsiReader(nFile int) *ansiReader {
	file, fd := GetStdFile(nFile)
	return &ansiReader{
		file:           file,
		fd:             fd,
		command:        make([]byte, 0, ANSI_MAX_CMD_LENGTH),
		escapeSequence: []byte(KEY_ESC_CSI),
		buffer:         make([]byte, 0),
	}
}

// Close closes the wrapped file.
func (ar *ansiReader) Close() (err error) {
	return ar.file.Close()
}

// Fd returns the file descriptor of the wrapped file.
func (ar *ansiReader) Fd() uintptr {
	return ar.fd
}

// Read reads up to len(p) bytes of translated input events into p.
func (ar *ansiReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// Previously read bytes exist, read as much as we can and return
	if len(ar.buffer) > 0 {
		logger.Debugf("Reading previously cached bytes")

		originalLength := len(ar.buffer)
		copiedLength := copy(p, ar.buffer)

		if copiedLength == originalLength {
			ar.buffer = make([]byte, 0, len(p))
		} else {
			ar.buffer = ar.buffer[copiedLength:]
		}

		logger.Debugf("Read from cache p[%d]: % x", copiedLength, p)
		return copiedLength, nil
	}

	// Read and translate key events
	events, err := readInputEvents(ar.fd, len(p))
	if err != nil {
		return 0, err
	} else if len(events) == 0 {
		logger.Debug("No input events detected")
		return 0, nil
	}

	keyBytes := translateKeyEvents(events, ar.escapeSequence)

	// Save excess bytes and right-size keyBytes
	if len(keyBytes) > len(p) {
		logger.Debugf("Received %d keyBytes, only room for %d bytes", len(keyBytes), len(p))
		ar.buffer = keyBytes[len(p):]
		keyBytes = keyBytes[:len(p)]
	} else if len(keyBytes) == 0 {
		logger.Debug("No key bytes returned from the translator")
		return 0, nil
	}

	copiedLength := copy(p, keyBytes)
	if copiedLength != len(keyBytes) {
		return 0, errors.New("Unexpected copy length encountered.")
	}

	logger.Debugf("Read        p[%d]: % x", copiedLength, p)
	logger.Debugf("Read keyBytes[%d]: % x", copiedLength, keyBytes)
	return copiedLength, nil
}

// readInputEvents polls until at least one event is available.
func readInputEvents(fd uintptr, maxBytes int) ([]INPUT_RECORD, error) {
	// Determine the maximum number of records to retrieve
	// -- Cast around the type system to obtain the size of a single INPUT_RECORD.
	//    unsafe.Sizeof requires an expression vs. a type-reference; the casting
	//    tricks the type system into believing it has such an expression.
	recordSize := int(unsafe.Sizeof(*((*INPUT_RECORD)(unsafe.Pointer(&maxBytes)))))
	countRecords := maxBytes / recordSize
	if countRecords > MAX_INPUT_EVENTS {
		countRecords = MAX_INPUT_EVENTS
	}
	logger.Debugf("[windows] readInputEvents: Reading %v records (buffer size %v, record size %v)", countRecords, maxBytes, recordSize)

	// Wait for and read input events
	events := make([]INPUT_RECORD, countRecords)
	nEvents := uint32(0)
	eventsExist, err := WaitForSingleObject(fd, WAIT_INFINITE)
	if err != nil {
		return nil, err
	}

	if eventsExist {
		err = ReadConsoleInput(fd, events, &nEvents)
		if err != nil {
			return nil, err
		}
	}

	// Return a slice restricted to the number of returned records
	logger.Debugf("[windows] readInputEvents: Read %v events", nEvents)
	return events[:nEvents], nil
}

// KeyEvent Translation Helpers

var arrowKeyMapPrefix = map[WORD]string{
	VK_UP:    "%s%sA",
	VK_DOWN:  "%s%sB",
	VK_RIGHT: "%s%sC",
	VK_LEFT:  "%s%sD",
}

var keyMapPrefix = map[WORD]string{
	VK_UP:     "\x1B[%sA",
	VK_DOWN:   "\x1B[%sB",
	VK_RIGHT:  "\x1B[%sC",
	VK_LEFT:   "\x1B[%sD",
	VK_HOME:   "\x1B[1%s~", // showkey shows ^[[1
	VK_END:    "\x1B[4%s~", // showkey shows ^[[4
	VK_INSERT: "\x1B[2%s~",
	VK_DELETE: "\x1B[3%s~",
	VK_PRIOR:  "\x1B[5%s~",
	VK_NEXT:   "\x1B[6%s~",
	VK_F1:     "",
	VK_F2:     "",
	VK_F3:     "\x1B[13%s~",
	VK_F4:     "\x1B[14%s~",
	VK_F5:     "\x1B[15%s~",
	VK_F6:     "\x1B[17%s~",
	VK_F7:     "\x1B[18%s~",
	VK_F8:     "\x1B[19%s~",
	VK_F9:     "\x1B[20%s~",
	VK_F10:    "\x1B[21%s~",
	VK_F11:    "\x1B[23%s~",
	VK_F12:    "\x1B[24%s~",
}

// translateKeyEvents converts the input events into the appropriate ANSI string.
func translateKeyEvents(events []INPUT_RECORD, escapeSequence []byte) []byte {
	var buffer bytes.Buffer
	for _, event := range events {
		if event.EventType == KEY_EVENT && event.KeyEvent.KeyDown != 0 {
			buffer.WriteString(keyToString(&event.KeyEvent, escapeSequence))
		}
	}

	return buffer.Bytes()
}

// keyToString maps the given input event record to the corresponding string.
func keyToString(keyEvent *KEY_EVENT_RECORD, escapeSequence []byte) string {
	if keyEvent.UnicodeChar == 0 {
		return formatVirtualKey(keyEvent.VirtualKeyCode, keyEvent.ControlKeyState, escapeSequence)
	}

	_, alt, control := getControlKeys(keyEvent.ControlKeyState)
	if control {
		// TODO(azlinux): Implement following control sequences
		// <Ctrl>-D  Signals the end of input from the keyboard; also exits current shell.
		// <Ctrl>-H  Deletes the first character to the left of the cursor. Also called the ERASE key.
		// <Ctrl>-Q  Restarts printing after it has been stopped with <Ctrl>-s.
		// <Ctrl>-S  Suspends printing on the screen (does not stop the program).
		// <Ctrl>-U  Deletes all characters on the current line. Also called the KILL key.
		// <Ctrl>-E  Quits current command and creates a core

	}

	// <Alt>+Key generates ESC N Key
	if !control && alt {
		return KEY_ESC_N + strings.ToLower(string(keyEvent.UnicodeChar))
	}

	return string(keyEvent.UnicodeChar)
}

// formatVirtualKey converts a virtual key (e.g., up arrow) into the appropriate ANSI string.
func formatVirtualKey(key WORD, controlState DWORD, escapeSequence []byte) string {
	shift, alt, control := getControlKeys(controlState)
	modifier := getControlKeysModifier(shift, alt, control, false)

	if format, ok := arrowKeyMapPrefix[key]; ok {
		return fmt.Sprintf(format, escapeSequence, modifier)
	}

	if format, ok := keyMapPrefix[key]; ok {
		return fmt.Sprintf(format, modifier)
	}

	return ""
}

// getControlKeys extracts the shift, alt, and ctrl key states.
func getControlKeys(controlState DWORD) (shift, alt, control bool) {
	shift = 0 != (controlState & SHIFT_PRESSED)
	alt = 0 != (controlState & (LEFT_ALT_PRESSED | RIGHT_ALT_PRESSED))
	control = 0 != (controlState & (LEFT_CTRL_PRESSED | RIGHT_CTRL_PRESSED))
	return shift, alt, control
}

// getControlKeysModifier returns the ANSI modifier for the given combination of control keys.
func getControlKeysModifier(shift, alt, control, meta bool) string {
	if shift && alt && control {
		return KEY_CONTROL_PARAM_8
	}
	if alt && control {
		return KEY_CONTROL_PARAM_7
	}
	if shift && control {
		return KEY_CONTROL_PARAM_6
	}
	if control {
		return KEY_CONTROL_PARAM_5
	}
	if shift && alt {
		return KEY_CONTROL_PARAM_4
	}
	if alt {
		return KEY_CONTROL_PARAM_3
	}
	if shift {
		return KEY_CONTROL_PARAM_2
	}
	return ""
}