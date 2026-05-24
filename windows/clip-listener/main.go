// clip-listener.exe — event-driven Windows clipboard image bridge.
//
// Listens for WM_CLIPBOARDUPDATE via AddClipboardFormatListener on a
// message-only window. When an image lands on the clipboard, encodes it as
// PNG via GDI+ (so Windows handles BI_BITFIELDS BMPs natively) and emits
// "IMAGE <linux-path>" on stdout for the WSL-side consumer.
//
// Single-instance: a named mutex Local\WSLClipBridgeListener — a second
// invocation exits cleanly. Stdin is expected to be redirected to NUL by
// the caller so the process does not steal keystrokes from the parent tty.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

const (
	wmDestroy          = 0x0002
	wmClipboardUpdate  = 0x031D
	cfBitmap           = 2
	gdiplusOk          = 0
	errorAlreadyExists = 183

	// Events firing within dedupWindow of our last emit are assumed to be
	// the WSLg round-trip from our own wl-copy and are suppressed. Slower
	// repeat events (e.g. a user re-copying the same image) fall through
	// and are re-emitted so the Wayland clipboard is re-asserted.
	dedupWindow = 3 * time.Second
)

// HWND_MESSAGE = (HWND)-3. uintptr-sized two's complement.
var hwndMessage = ^uintptr(2)

// PNG encoder CLSID: {557CF406-1A04-11D3-9A73-0000F81EF32E}
var pngEncoderCLSID = guid{
	Data1: 0x557CF406,
	Data2: 0x1A04,
	Data3: 0x11D3,
	Data4: [8]byte{0x9A, 0x73, 0x00, 0x00, 0xF8, 0x1E, 0xF3, 0x2E},
}

type guid struct {
	Data1 uint32
	Data2 uint16
	Data3 uint16
	Data4 [8]byte
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

type gdiplusStartupInput struct {
	GdiplusVersion           uint32
	DebugEventCallback       uintptr
	SuppressBackgroundThread int32
	SuppressExternalCodecs   int32
}

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdiplus  = syscall.NewLazyDLL("gdiplus.dll")

	pRegisterClassExW              = user32.NewProc("RegisterClassExW")
	pCreateWindowExW               = user32.NewProc("CreateWindowExW")
	pDefWindowProcW                = user32.NewProc("DefWindowProcW")
	pGetMessageW                   = user32.NewProc("GetMessageW")
	pTranslateMessage              = user32.NewProc("TranslateMessage")
	pDispatchMessageW              = user32.NewProc("DispatchMessageW")
	pPostQuitMessage               = user32.NewProc("PostQuitMessage")
	pAddClipboardFormatListener    = user32.NewProc("AddClipboardFormatListener")
	pRemoveClipboardFormatListener = user32.NewProc("RemoveClipboardFormatListener")
	pOpenClipboard                 = user32.NewProc("OpenClipboard")
	pCloseClipboard                = user32.NewProc("CloseClipboard")
	pIsClipboardFormatAvailable    = user32.NewProc("IsClipboardFormatAvailable")
	pGetClipboardData              = user32.NewProc("GetClipboardData")

	pGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")
	pCreateMutexW     = kernel32.NewProc("CreateMutexW")
	pGetLastError     = kernel32.NewProc("GetLastError")

	pGdiplusStartup              = gdiplus.NewProc("GdiplusStartup")
	pGdiplusShutdown             = gdiplus.NewProc("GdiplusShutdown")
	pGdipCreateBitmapFromHBITMAP = gdiplus.NewProc("GdipCreateBitmapFromHBITMAP")
	pGdipSaveImageToFile         = gdiplus.NewProc("GdipSaveImageToFile")
	pGdipDisposeImage            = gdiplus.NewProc("GdipDisposeImage")
)

var (
	winOutDir    string
	linuxOutDir  string
	counter      uint64
	lastHash     string
	lastEmitTime time.Time
)

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "[%s] %s\n", time.Now().Format(time.RFC3339), fmt.Sprintf(format, args...))
}

func main() {
	// Win32 message loops are tied to the thread that created the window.
	runtime.LockOSThread()

	flag.StringVar(&winOutDir, "win-out", "", "Windows-side directory to save PNGs (Windows path)")
	flag.StringVar(&linuxOutDir, "linux-out", "", "Linux-side path prefix to emit on stdout (same physical dir)")
	flag.Parse()
	if winOutDir == "" || linuxOutDir == "" {
		fmt.Fprintln(os.Stderr, "usage: clip-listener.exe --win-out <path> --linux-out <path>")
		os.Exit(2)
	}

	// Single-instance guard via named mutex.
	mutexName, _ := syscall.UTF16PtrFromString(`Local\WSLClipBridgeListener`)
	if _, _, _ = pCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(mutexName))); true {
		lastErr, _, _ := pGetLastError.Call()
		if lastErr == errorAlreadyExists {
			fmt.Fprintln(os.Stderr, "another clip-listener.exe is already running; exiting")
			os.Exit(0)
		}
	}

	if err := os.MkdirAll(winOutDir, 0o755); err != nil {
		die("mkdir %q: %v", winOutDir, err)
	}

	// GDI+ startup.
	var gdipToken uintptr
	startup := gdiplusStartupInput{GdiplusVersion: 1}
	r, _, _ := pGdiplusStartup.Call(
		uintptr(unsafe.Pointer(&gdipToken)),
		uintptr(unsafe.Pointer(&startup)),
		0,
	)
	if r != gdiplusOk {
		die("GdiplusStartup: status %d", r)
	}
	defer pGdiplusShutdown.Call(gdipToken)

	// Register the window class.
	className, _ := syscall.UTF16PtrFromString("WSLClipBridgeListener")
	hInstance, _, _ := pGetModuleHandleW.Call(0)
	wc := wndClassEx{
		Size:      uint32(unsafe.Sizeof(wndClassEx{})),
		WndProc:   syscall.NewCallback(wndProc),
		Instance:  hInstance,
		ClassName: className,
	}
	atom, _, err := pRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	if atom == 0 {
		die("RegisterClassExW: %v", err)
	}

	// Create a message-only window (parent = HWND_MESSAGE).
	windowName, _ := syscall.UTF16PtrFromString("WSL Clip Bridge")
	hwnd, _, err := pCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(windowName)),
		0, 0, 0, 0, 0,
		hwndMessage,
		0, hInstance, 0,
	)
	if hwnd == 0 {
		die("CreateWindowExW: %v", err)
	}

	r, _, err = pAddClipboardFormatListener.Call(hwnd)
	if r == 0 {
		die("AddClipboardFormatListener: %v", err)
	}

	logf("clip-listener started; win-out=%s linux-out=%s", winOutDir, linuxOutDir)

	var m msg
	for {
		ret, _, _ := pGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) == -1 {
			die("GetMessageW returned -1")
		}
		if ret == 0 {
			break
		}
		pTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		pDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func wndProc(hwnd, message, wparam, lparam uintptr) uintptr {
	switch message {
	case wmClipboardUpdate:
		handleClipboardUpdate(hwnd)
		return 0
	case wmDestroy:
		pRemoveClipboardFormatListener.Call(hwnd)
		pPostQuitMessage.Call(0)
		return 0
	}
	r, _, _ := pDefWindowProcW.Call(hwnd, message, wparam, lparam)
	return r
}

func handleClipboardUpdate(hwnd uintptr) {
	logf("event: WM_CLIPBOARDUPDATE")

	if r, _, _ := pIsClipboardFormatAvailable.Call(cfBitmap); r == 0 {
		logf("  skip: no CF_BITMAP on clipboard")
		return
	}
	if r, _, _ := pOpenClipboard.Call(hwnd); r == 0 {
		logf("  skip: OpenClipboard failed")
		return
	}
	defer pCloseClipboard.Call()

	hbitmap, _, _ := pGetClipboardData.Call(cfBitmap)
	if hbitmap == 0 {
		logf("  skip: GetClipboardData(CF_BITMAP) returned NULL")
		return
	}

	var gpImage uintptr
	r, _, _ := pGdipCreateBitmapFromHBITMAP.Call(hbitmap, 0, uintptr(unsafe.Pointer(&gpImage)))
	if r != gdiplusOk || gpImage == 0 {
		logf("  skip: GdipCreateBitmapFromHBITMAP status %d", r)
		return
	}
	defer pGdipDisposeImage.Call(gpImage)

	counter++
	name := fmt.Sprintf("clip-%d.png", counter)
	winPath := filepath.Join(winOutDir, name)
	linuxPath := linuxOutDir + "/" + name

	winPathW, err := syscall.UTF16PtrFromString(winPath)
	if err != nil {
		logf("  skip: UTF16 conversion failed: %v", err)
		return
	}
	r, _, _ = pGdipSaveImageToFile.Call(
		gpImage,
		uintptr(unsafe.Pointer(winPathW)),
		uintptr(unsafe.Pointer(&pngEncoderCLSID)),
		0,
	)
	if r != gdiplusOk {
		logf("  skip: GdipSaveImageToFile status %d (path %s)", r, winPath)
		return
	}

	data, err := os.ReadFile(winPath)
	if err != nil {
		logf("  skip: ReadFile %s failed: %v", winPath, err)
		return
	}
	sum := sha256.Sum256(data)
	h := hex.EncodeToString(sum[:])

	// Hash match within the dedup window is the WSLg round-trip from our
	// own wl-copy. A hash match outside the window is a real user re-copy
	// (the same image) — we must re-emit to re-claim the Wayland selection
	// after WSLg has overwritten it with image/bmp.
	if h == lastHash && time.Since(lastEmitTime) < dedupWindow {
		logf("  dedup: hash matches last emit %.2fs ago (WSLg round-trip)", time.Since(lastEmitTime).Seconds())
		_ = os.Remove(winPath)
		return
	}

	lastHash = h
	lastEmitTime = time.Now()

	if _, err := fmt.Printf("IMAGE %s\n", linuxPath); err != nil {
		// Parent pipe is gone — nothing left to do.
		os.Exit(0)
	}
	logf("  emit: %s (%d bytes, hash %s)", linuxPath, len(data), h[:8])
}

func die(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
