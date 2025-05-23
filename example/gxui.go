// gxui.go
package main

import (
	"fmt"
	"image"
	"image/draw"
	"log/slog"
	"os"
	"strings"

	"github.com/google/gxui/drivers/gl"

	"github.com/google/gxui"
	"github.com/google/gxui/samples/flags"
	"github.com/google/gxui/themes/light"

	"github.com/nakagami/grdp"
)

var (
	gc                     *grdp.RdpClient
	driverc                gxui.Driver
	width, height          int
	ScreenImage            *image.RGBA
	img                    gxui.Image
	BitmapCH               chan []grdp.Bitmap
	lastMouseX, lastMouseY int
)

func uiRdp(info *Info) (error, *grdp.RdpClient) {
	BitmapCH = make(chan []grdp.Bitmap, 500)
	g := grdp.NewRdpClient(fmt.Sprintf("%s:%s", info.Host, info.Port), info.Width, info.Height)
	err := g.Login(info.Domain, info.Username, info.Password)
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
	}).OnBitmap(ui_paint_bitmap)

	return nil, g
}

func appMain(driver gxui.Driver) {
	theme := light.CreateTheme(driver)
	window := theme.CreateWindow(width, height, "MSTSC")
	window.SetScale(flags.DefaultScaleFactor)

	img = theme.CreateImage()

	layoutImg := theme.CreateLinearLayout()
	layoutImg.SetSizeMode(gxui.Fill)
	layoutImg.SetHorizontalAlignment(gxui.AlignCenter)
	layoutImg.AddChild(img)
	layoutImg.SetVisible(true)
	ScreenImage = image.NewRGBA(image.Rect(0, 0, width, height))
	layoutImg.OnMouseDown(func(e gxui.MouseEvent) {
		gc.MouseDown(int(e.Button), e.Point.X, e.Point.Y)
	})
	layoutImg.OnMouseUp(func(e gxui.MouseEvent) {
		gc.MouseUp(int(e.Button), lastMouseX, lastMouseY)
		//		gc.MouseUp(int(e.Button), e.Point.X, e.Point.Y)
	})
	layoutImg.OnMouseMove(func(e gxui.MouseEvent) {
		gc.MouseMove(e.Point.X, e.Point.Y)
		lastMouseX = e.Point.X
		lastMouseY = e.Point.Y
	})
	layoutImg.OnMouseScroll(func(e gxui.MouseEvent) {
		gc.MouseWheel(e.ScrollY, e.Point.X, e.Point.Y)
	})
	window.OnKeyDown(func(e gxui.KeyboardEvent) {
		if gc == nil {
			return
		}
		key := transKey(e.Key)
		gc.KeyDown(key, "")
	})
	window.OnKeyUp(func(e gxui.KeyboardEvent) {
		if gc == nil {
			return
		}
		key := transKey(e.Key)
		gc.KeyUp(key, "")
	})

	h := strings.Join([]string{os.Getenv("GRDP_HOST"), ":", os.Getenv("GRDP_PORT")}, "")
	u := os.Getenv("GRDP_USER")
	p := os.Getenv("GRDP_PASSWORD")

	err, info := NewInfo(h, u, p)
	info.Width, info.Height = width, height
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	driverc = driver
	err, gc = uiRdp(info)
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	layoutImg.SetVisible(true)

	window.AddChild(layoutImg)
	window.OnClose(func() {
		if gc != nil {
			gc.Close()
		}

		driver.Terminate()
	})
	update()
}

func update() {
	go func() {
		for {
			select {
			case bs := <-BitmapCH:
				paint_bitmap(bs)
			default:
			}
		}
	}()
}

func paint_bitmap(bs []grdp.Bitmap) {
	for _, bm := range bs {
		m := bm.BitmapToRGBA()
		draw.Draw(ScreenImage, ScreenImage.Bounds().Add(image.Pt(bm.DestLeft, bm.DestTop)), m, m.Bounds().Min, draw.Src)
	}

	driverc.Call(func() {
		texture := driverc.CreateTexture(ScreenImage, 1)
		img.SetTexture(texture)
	})

}

func ui_paint_bitmap(bs []grdp.Bitmap) {
	BitmapCH <- bs
}

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

func transKey(in gxui.KeyboardKey) int {
	if v, ok := KeyMap[in]; ok {
		return v
	}
	return 0
}

func main() {
	//    handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})
	//    slog.SetDefault(slog.New(handler))

	width, height = 1280, 1024
	gl.StartDriver(appMain)
}

type Info struct {
	Domain   string
	Host     string
	Port     string
	Username string
	Password string
	Height   int
	Width    int
}

func NewInfo(ip, user, password string) (error, *Info) {
	var i Info
	if ip == "" || user == "" || password == "" {
		return fmt.Errorf("Must ip/user/password"), nil
	}
	t := strings.Split(ip, ":")
	i.Host = t[0]
	i.Port = "3389"
	if len(t) > 1 {
		i.Port = t[1]
	}
	if strings.Index(user, "\\") != -1 {
		t = strings.Split(user, "\\")
		i.Domain = t[0]
		i.Username = t[len(t)-1]
	} else if strings.Index(user, "/") != -1 {
		t = strings.Split(user, "/")
		i.Domain = t[0]
		i.Username = t[len(t)-1]
	} else {
		i.Username = user
	}

	i.Password = password

	return nil, &i
}
