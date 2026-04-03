// gxui.go
package main

import (
	"bytes"
	"fmt"
	"image"
	"image/draw"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ebitengine/oto/v3"
	"github.com/google/gxui"
	"github.com/google/gxui/drivers/gl"
	"github.com/google/gxui/samples/flags"
	"github.com/google/gxui/themes/light"
	"golang.design/x/clipboard"

	"github.com/nakagami/grdp"
	"github.com/nakagami/grdp/plugin/rdpsnd"
)

// audioStream is a thread-safe buffer that bridges the RDPSND OnAudio
// callback (producer) and the oto audio player (consumer).
//
// Unlike a blocking io.Reader, Read returns silence (zeros) when the buffer
// is empty.  This keeps the oto player running continuously and avoids
// audio-device stalls that would cause clicks/pops when data resumes.
// Short cross-fades are applied at underrun boundaries (like rdpyqt).
type audioStream struct {
	mu          sync.Mutex
	buf         bytes.Buffer
	closed      bool
	wasUnderrun bool
}

const maxAudioBuf = 1 << 20 // 1 MB ≈ 6 s of PCM 44100 Hz / 2 ch / 16 bit

// fadeFrames is the number of stereo frames used for fade-in / fade-out
// at underrun boundaries (~1.5 ms at 44100 Hz).
const fadeFrames = 64

func newAudioStream() *audioStream {
	return &audioStream{}
}

func (s *audioStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.buf.Len() > maxAudioBuf {
		s.buf.Reset()
	}
	return s.buf.Write(p)
}

// Read fills p with audio data.  If the internal buffer is empty the
// slice is zero-filled (silence) so the oto player never stalls.
func (s *audioStream) Read(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed && s.buf.Len() == 0 {
		return 0, io.EOF
	}

	if s.buf.Len() == 0 {
		// No data available — return silence instead of blocking.
		s.wasUnderrun = true
		clear(p)
		return len(p), nil
	}

	n, err := s.buf.Read(p)

	if s.wasUnderrun {
		applyFade(p[:n], true) // fade-in to avoid click
		s.wasUnderrun = false
	}
	if s.buf.Len() == 0 {
		applyFade(p[:n], false) // fade-out before silence
	}

	return n, err
}

func (s *audioStream) Close() {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
}

// Reset flushes all buffered audio data. Called when the server closes the
// audio channel (e.g. media seek) so stale audio does not keep playing.
func (s *audioStream) Reset() {
	s.mu.Lock()
	s.buf.Reset()
	s.mu.Unlock()
}

// applyFade applies a short linear fade to 16-bit stereo PCM data.
// fadeIn=true ramps the first fadeFrames from 0→1; fadeIn=false ramps
// the last fadeFrames from 1→0.
func applyFade(data []byte, fadeIn bool) {
	const frameSize = 4 // 2 bytes × 2 channels
	totalFrames := len(data) / frameSize
	n := totalFrames
	if n > fadeFrames {
		n = fadeFrames
	}
	if n <= 0 {
		return
	}
	for i := 0; i < n; i++ {
		var off int
		var num int
		if fadeIn {
			off = i * frameSize
			num = i
		} else {
			off = (totalFrames - n + i) * frameSize
			num = n - 1 - i
		}
		for ch := 0; ch < 2; ch++ {
			soff := off + ch*2
			sample := int16(uint16(data[soff]) | uint16(data[soff+1])<<8)
			sample = int16(int(sample) * num / n)
			data[soff] = byte(uint16(sample))
			data[soff+1] = byte(uint16(sample) >> 8)
		}
	}
}

// --- System clipboard helpers (golang.design/x/clipboard) ------------------

func readSystemClipboard() string {
	b := clipboard.Read(clipboard.FmtText)
	if b == nil {
		return ""
	}
	return string(b)
}

func writeSystemClipboard(text string) {
	clipboard.Write(clipboard.FmtText, []byte(text))
}

// clipboardWatcher polls the system clipboard and notifies the RDP server
// when the local clipboard changes.
func clipboardWatcher(client *grdp.RdpClient, stopCh <-chan struct{}) {
	lastText := readSystemClipboard()
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-stopCh:
			return
		case <-ticker.C:
			text := readSystemClipboard()
			if text != lastText {
				lastText = text
				client.NotifyClipboardChanged()
			}
		}
	}
}

var (
	rdpClient              *grdp.RdpClient
	driverc                gxui.Driver
	screenImage            *image.RGBA
	screenMu               sync.Mutex
	img                    gxui.Image
	bitmapCH               chan []grdp.Bitmap
	lastMouseX, lastMouseY int
	resizeTimer            *time.Timer
	audioStr               *audioStream
	otoCtx                 *oto.Context
	otoPlayer              *oto.Player
	clipStopCh             chan struct{}
)

func uiRdp(hostPort, domain, user, password string, width, height int, keyboardType, keyboardLayout string) (error, *grdp.RdpClient) {
	bitmapCH = make(chan []grdp.Bitmap, 500)
	g := grdp.NewRdpClient(hostPort, width, height)
	if keyboardType != "" {
		g.SetKeyboardType(keyboardType)
	}
	if keyboardLayout != "" {
		g.SetKeyboardLayout(keyboardLayout)
	}

	g.OnAudio(func(af rdpsnd.AudioFormat, data []byte) {
		audioStr.Write(data)
	})

	g.OnAudioReset(func() {
		slog.Debug("Audio reset: flushing playback buffer")
		audioStr.Reset()
	})

	g.OnClipboard(
		func(text string) {
			slog.Debug("clipboard: remote → local", "len", len(text))
			writeSystemClipboard(text)
		},
		func() string {
			text := readSystemClipboard()
			slog.Debug("clipboard: local → remote", "len", len(text))
			return text
		},
	)

	err := g.Login(domain, user, password)
	if err != nil {
		slog.Error("Login", "err", err)
		return err, nil
	}

	g.OnError(func(e error) {
		slog.Debug("on error", "err", e)
	}).OnClose(func() {
		slog.Debug("on close")
	}).OnSucces(func() {
		slog.Debug("on success")
	}).OnReady(func() {
		slog.Debug("on ready")
	}).OnPointerHide(func() {
		slog.Debug("on pointer_hide")
	}).OnPointerCached(func(idx uint16) {
		slog.Debug("on pointer_cached", "idx", idx)
	}).OnPointerUpdate(func(idx uint16, bpp uint16, x uint16, y uint16, width uint16, height uint16, mask []byte, data []byte) {
		slog.Debug("on pointer_update", "idx", idx)
	}).OnBitmap(func(bs []grdp.Bitmap) {
		// Bitmap.Data for compressed bitmaps is borrowed from an internal
		// pool and only valid for the duration of this callback.  Copy the
		// pixel data before sending it to the asynchronous paint goroutine.
		for i := range bs {
			d := make([]byte, len(bs[i].Data))
			copy(d, bs[i].Data)
			bs[i].Data = d
		}
		// Non-blocking send: drop frames if the paint goroutine can't
		// keep up.  This prevents the decode goroutine from blocking on
		// downstream rendering, which would stall frame ACKs.
		select {
		case bitmapCH <- bs:
		default:
		}
	})

	return nil, g
}

func appMain(driver gxui.Driver) {
	hostPort := strings.Join([]string{os.Getenv("GRDP_HOST"), os.Getenv("GRDP_PORT")}, ":")
	domain := os.Getenv("GRDP_DOMAIN")
	user := os.Getenv("GRDP_USER")
	password := os.Getenv("GRDP_PASSWORD")

	var width, height int
	_, err := fmt.Sscanf(os.Getenv("GRDP_WINDOW_SIZE"), "%dx%d", &width, &height)
	if err != nil {
		width, height = 1280, 800
	}
	keyboardType := os.Getenv("GRDP_KEYBOARD_TYPE")
	keyboardLayout := os.Getenv("GRDP_KEYBOARD_LAYOUT")

	audioStr = newAudioStream()

	if err := clipboard.Init(); err != nil {
		slog.Error("Clipboard init failed", "err", err)
	}

	err, rdpClient = uiRdp(hostPort, domain, user, password, width, height, keyboardType, keyboardLayout)
	if err != nil {
		fmt.Println(err.Error())
		driver.Terminate()
		return
	}

	// Start clipboard watcher goroutine
	clipStopCh = make(chan struct{})
	go clipboardWatcher(rdpClient, clipStopCh)

	// Initialize audio output (PCM 44100Hz stereo 16-bit)
	otoOp := &oto.NewContextOptions{
		SampleRate:   44100,
		ChannelCount: 2,
		Format:       oto.FormatSignedInt16LE,
	}
	ctx, readyCh, otoErr := oto.NewContext(otoOp)
	if otoErr != nil {
		slog.Error("Audio output init failed", "err", otoErr)
	} else {
		<-readyCh
		otoCtx = ctx
		otoPlayer = otoCtx.NewPlayer(audioStr)
		otoPlayer.Play()
		slog.Debug("Audio output initialized")
	}

	theme := light.CreateTheme(driver)
	window := theme.CreateWindow(width, height, "MSTSC")
	window.SetScale(flags.DefaultScaleFactor)

	img = theme.CreateImage()

	layoutImg := theme.CreateLinearLayout()
	layoutImg.SetSizeMode(gxui.Fill)
	layoutImg.SetHorizontalAlignment(gxui.AlignCenter)
	layoutImg.AddChild(img)
	layoutImg.SetVisible(true)
	screenImage = image.NewRGBA(image.Rect(0, 0, width, height))

	layoutImg.OnMouseDown(func(e gxui.MouseEvent) {
		if rdpClient == nil {
			return
		}
		rdpClient.MouseDown(int(e.Button), e.Point.X, e.Point.Y)
	})
	layoutImg.OnMouseUp(func(e gxui.MouseEvent) {
		if rdpClient == nil {
			return
		}
		rdpClient.MouseUp(int(e.Button), lastMouseX, lastMouseY)
		//		rdpClient.MouseUp(int(e.Button), e.Point.X, e.Point.Y)
	})
	layoutImg.OnMouseMove(func(e gxui.MouseEvent) {
		if rdpClient == nil {
			return
		}
		rdpClient.MouseMove(e.Point.X, e.Point.Y)
		lastMouseX = e.Point.X
		lastMouseY = e.Point.Y
	})
	layoutImg.OnMouseScroll(func(e gxui.MouseEvent) {
		if rdpClient == nil {
			return
		}
		rdpClient.MouseWheel(e.ScrollY)
	})
	window.OnKeyDown(func(e gxui.KeyboardEvent) {
		if rdpClient == nil {
			return
		}
		key := transKey(e.Key)
		rdpClient.KeyDown(key)
	})
	window.OnKeyUp(func(e gxui.KeyboardEvent) {
		if rdpClient == nil {
			return
		}
		key := transKey(e.Key)
		rdpClient.KeyUp(key)
	})

	driverc = driver
	layoutImg.SetVisible(true)
	window.AddChild(layoutImg)
	window.OnClose(func() {
		if clipStopCh != nil {
			close(clipStopCh)
		}
		if resizeTimer != nil {
			resizeTimer.Stop()
		}
		if otoPlayer != nil {
			otoPlayer.Pause()
		}
		if audioStr != nil {
			audioStr.Close()
		}
		if rdpClient != nil {
			rdpClient.Close()
		}

		driver.Terminate()
	})
	window.OnResize(func() {
		sz := layoutImg.Size()
		w, h := sz.W, sz.H
		if w <= 0 || h <= 0 {
			return
		}
		if w == rdpClient.Width() && h == rdpClient.Height() {
			return
		}
		if resizeTimer != nil {
			resizeTimer.Stop()
		}
		resizeTimer = time.AfterFunc(500*time.Millisecond, func() {
			slog.Debug("Window resized, reconnecting", "width", w, "height", h)
			if err := rdpClient.Reconnect(w, h); err != nil {
				slog.Error("Reconnect failed", "err", err)
				return
			}
			// Only replace the screen buffer after a successful reconnect
			// so the display keeps the last good frame on failure.
			screenMu.Lock()
			screenImage = image.NewRGBA(image.Rect(0, 0, w, h))
			screenMu.Unlock()
		})
	})
	update()
}

func update() {
	go func() {
		for bs := range bitmapCH {
			screenMu.Lock()
			paintBitmapsLocked(bs)
		drain:
			for {
				select {
				case more := <-bitmapCH:
					paintBitmapsLocked(more)
				default:
					break drain
				}
			}
			screenMu.Unlock()

			driverc.Call(func() {
				texture := driverc.CreateTexture(screenImage, 1)
				img.SetTexture(texture)
			})
		}
	}()
}

func paintBitmapsLocked(bs []grdp.Bitmap) {
	for _, bm := range bs {
		m := bm.RGBA()
		destRect := image.Rect(bm.DestLeft, bm.DestTop, bm.DestRight+1, bm.DestBottom+1)
		draw.Draw(screenImage, destRect, m, image.Pt(0, 0), draw.Src)
	}
}

func transKey(in gxui.KeyboardKey) int {
	var KeyMap = map[gxui.KeyboardKey]int{
		gxui.KeyUnknown:      0x0000,
		gxui.KeyEscape:       0x0001,
		gxui.Key1:            0x0002,
		gxui.Key2:            0x0003,
		gxui.Key3:            0x0004,
		gxui.Key4:            0x0005,
		gxui.Key5:            0x0006,
		gxui.Key6:            0x0007,
		gxui.Key7:            0x0008,
		gxui.Key8:            0x0009,
		gxui.Key9:            0x000A,
		gxui.Key0:            0x000B,
		gxui.KeyMinus:        0x000C,
		gxui.KeyEqual:        0x000D,
		gxui.KeyBackspace:    0x000E,
		gxui.KeyTab:          0x000F,
		gxui.KeyQ:            0x0010,
		gxui.KeyW:            0x0011,
		gxui.KeyE:            0x0012,
		gxui.KeyR:            0x0013,
		gxui.KeyT:            0x0014,
		gxui.KeyY:            0x0015,
		gxui.KeyU:            0x0016,
		gxui.KeyI:            0x0017,
		gxui.KeyO:            0x0018,
		gxui.KeyP:            0x0019,
		gxui.KeyLeftBracket:  0x001A,
		gxui.KeyRightBracket: 0x001B,
		gxui.KeyEnter:        0x001C,
		gxui.KeyLeftControl:  0x001D,
		gxui.KeyA:            0x001E,
		gxui.KeyS:            0x001F,
		gxui.KeyD:            0x0020,
		gxui.KeyF:            0x0021,
		gxui.KeyG:            0x0022,
		gxui.KeyH:            0x0023,
		gxui.KeyJ:            0x0024,
		gxui.KeyK:            0x0025,
		gxui.KeyL:            0x0026,
		gxui.KeySemicolon:    0x0027,
		gxui.KeyApostrophe:   0x0028,
		gxui.KeyGraveAccent:  0x0029,
		gxui.KeyLeftShift:    0x002A,
		gxui.KeyBackslash:    0x002B,
		gxui.KeyZ:            0x002C,
		gxui.KeyX:            0x002D,
		gxui.KeyC:            0x002E,
		gxui.KeyV:            0x002F,
		gxui.KeyB:            0x0030,
		gxui.KeyN:            0x0031,
		gxui.KeyM:            0x0032,
		gxui.KeyComma:        0x0033,
		gxui.KeyPeriod:       0x0034,
		gxui.KeySlash:        0x0035,
		gxui.KeyRightShift:   0x0036,
		gxui.KeyKpMultiply:   0x0037,
		gxui.KeyLeftAlt:      0x0038,
		gxui.KeySpace:        0x0039,
		gxui.KeyCapsLock:     0x003A,
		gxui.KeyF1:           0x003B,
		gxui.KeyF2:           0x003C,
		gxui.KeyF3:           0x003D,
		gxui.KeyF4:           0x003E,
		gxui.KeyF5:           0x003F,
		gxui.KeyF6:           0x0040,
		gxui.KeyF7:           0x0041,
		gxui.KeyF8:           0x0042,
		gxui.KeyF9:           0x0043,
		gxui.KeyF10:          0x0044,
		//gxui.KeyPause:        0x0045,
		gxui.KeyScrollLock:   0x0046,
		gxui.KeyKp7:          0x0047,
		gxui.KeyKp8:          0x0048,
		gxui.KeyKp9:          0x0049,
		gxui.KeyKpSubtract:   0x004A,
		gxui.KeyKp4:          0x004B,
		gxui.KeyKp5:          0x004C,
		gxui.KeyKp6:          0x004D,
		gxui.KeyKpAdd:        0x004E,
		gxui.KeyKp1:          0x004F,
		gxui.KeyKp2:          0x0050,
		gxui.KeyKp3:          0x0051,
		gxui.KeyKp0:          0x0052,
		gxui.KeyKpDecimal:    0x0053,
		gxui.KeyF11:          0x0057,
		gxui.KeyF12:          0x0058,
		gxui.KeyKpEqual:      0x0059,
		gxui.KeyKpEnter:      0xE01C,
		gxui.KeyRightControl: 0xE01D,
		gxui.KeyKpDivide:     0xE035,
		gxui.KeyPrintScreen:  0xE037,
		gxui.KeyRightAlt:     0xE038,
		gxui.KeyNumLock:      0xE045,
		gxui.KeyPause:        0xE046,
		gxui.KeyHome:         0xE047,
		gxui.KeyUp:           0xE048,
		gxui.KeyPageUp:       0xE049,
		gxui.KeyLeft:         0xE04B,
		gxui.KeyRight:        0xE04D,
		gxui.KeyEnd:          0xE04F,
		gxui.KeyDown:         0xE050,
		gxui.KeyPageDown:     0xE051,
		gxui.KeyInsert:       0xE052,
		gxui.KeyDelete:       0xE053,
		gxui.KeyMenu:         0xE05D,
	}
	if v, ok := KeyMap[in]; ok {
		return v
	}
	return 0
}

func main() {
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))
	gl.StartDriver(appMain)
}
