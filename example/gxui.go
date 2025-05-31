// gxui.go
package main

import (
	"fmt"
	"image"
	"image/draw"
	"log/slog"
	"os"
	"strings"

	"github.com/google/gxui"
	"github.com/google/gxui/drivers/gl"
	"github.com/google/gxui/samples/flags"
	"github.com/google/gxui/themes/light"

	"github.com/nakagami/grdp"
)

var (
	rdpClient              *grdp.RdpClient
	driverc                gxui.Driver
	screenImage            *image.RGBA
	img                    gxui.Image
	bitmapCH               chan []grdp.Bitmap
	lastMouseX, lastMouseY int
)

func uiRdp(hostPort, domain, user, password string, width, height int) (error, *grdp.RdpClient) {
	bitmapCH = make(chan []grdp.Bitmap, 500)
	g := grdp.NewRdpClient(hostPort, width, height)
	err := g.Login(domain, user, password)
	if err != nil {
		slog.Error("Login", "err", err)
		return err, nil
	}

	g.OnError(func(e error) {
		slog.Info("on error", "err", e)
	}).OnClose(func() {
		slog.Info("on close")
	}).OnSucces(func() {
		slog.Info("on success")
	}).OnReady(func() {
		slog.Info("on ready")
	}).OnBitmap(func(bs []grdp.Bitmap) {
		bitmapCH <- bs
	})

	return nil, g
}

func appMain(driver gxui.Driver) {
	hostPort := strings.Join([]string{os.Getenv("GRDP_HOST"), os.Getenv("GRDP_PORT")}, ":")
	domain := os.Getenv("GRDP_DOMAIN")
	user := os.Getenv("GRDP_USER")
	password := os.Getenv("GRDP_PASSWORD")
	width := 1280
	height := 800

	err, rdpClient := uiRdp(hostPort, domain, user, password, width, height)
	if err != nil {
		fmt.Println(err.Error())
		return
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
		rdpClient.MouseWheel(e.ScrollY, e.Point.X, e.Point.Y)
	})
	window.OnKeyDown(func(e gxui.KeyboardEvent) {
		if rdpClient == nil {
			return
		}
		key := transKey(e.Key)
		rdpClient.KeyDown(key, "")
	})
	window.OnKeyUp(func(e gxui.KeyboardEvent) {
		if rdpClient == nil {
			return
		}
		key := transKey(e.Key)
		rdpClient.KeyUp(key, "")
	})

	driverc = driver
	layoutImg.SetVisible(true)
	window.AddChild(layoutImg)
	window.OnClose(func() {
		if rdpClient != nil {
			rdpClient.Close()
		}

		driver.Terminate()
	})
	update()
}

func update() {
	go func() {
		for {
			select {
			case bs := <-bitmapCH:
				paint_bitmap(bs)
			default:
			}
		}
	}()
}

func paint_bitmap(bs []grdp.Bitmap) {
	for _, bm := range bs {
		m := bm.RGBA()
		draw.Draw(screenImage, screenImage.Bounds().Add(image.Pt(bm.DestLeft, bm.DestTop)), m, m.Bounds().Min, draw.Src)
	}

	driverc.Call(func() {
		texture := driverc.CreateTexture(screenImage, 1)
		img.SetTexture(texture)
	})
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
		//"Quote":              0x0028,
		//"Backquote":          0x0029,
		gxui.KeyLeftShift:  0x002A,
		gxui.KeyBackslash:  0x002B,
		gxui.KeyZ:          0x002C,
		gxui.KeyX:          0x002D,
		gxui.KeyC:          0x002E,
		gxui.KeyV:          0x002F,
		gxui.KeyB:          0x0030,
		gxui.KeyN:          0x0031,
		gxui.KeyM:          0x0032,
		gxui.KeyComma:      0x0033,
		gxui.KeyPeriod:     0x0034,
		gxui.KeySlash:      0x0035,
		gxui.KeyRightShift: 0x0036,
		gxui.KeyKpMultiply: 0x0037,
		gxui.KeyLeftAlt:    0x0038,
		gxui.KeySpace:      0x0039,
		gxui.KeyCapsLock:   0x003A,
		gxui.KeyF1:         0x003B,
		gxui.KeyF2:         0x003C,
		gxui.KeyF3:         0x003D,
		gxui.KeyF4:         0x003E,
		gxui.KeyF5:         0x003F,
		gxui.KeyF6:         0x0040,
		gxui.KeyF7:         0x0041,
		gxui.KeyF8:         0x0042,
		gxui.KeyF9:         0x0043,
		gxui.KeyF10:        0x0044,
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
	//    handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	//    slog.SetDefault(slog.New(handler))
	gl.StartDriver(appMain)
}
