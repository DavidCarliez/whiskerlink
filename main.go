package main

import (
	"embed"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"log"
	"os"
	"runtime"

	"github.com/DavidCarliez/whiskerlink/internal/domain"
	"github.com/DavidCarliez/whiskerlink/internal/storage"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed build/appicon.png
var appIcon []byte

func init() {
	application.RegisterEvent[domain.Snapshot]("snapshot")
}

func main() {
	if runtime.GOOS == "linux" {
		if _, configured := os.LookupEnv("WEBKIT_DISABLE_DMABUF_RENDERER"); !configured {
			_ = os.Setenv("WEBKIT_DISABLE_DMABUF_RENDERER", "1")
		}
	}

	store, err := storage.Open()
	if err != nil {
		log.Fatal(err)
	}
	service, err := NewAppService(store)
	if err != nil {
		_ = store.Close()
		log.Fatal(err)
	}

	app := application.New(application.Options{
		Name:        "WhiskerLink",
		Description: "Private paths for files and services, powered by Tailcat",
		Services: []application.Service{
			application.NewService(service),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Windows: application.WindowsOptions{DisableQuitOnLastWindowClosed: true},
		Linux:   application.LinuxOptions{DisableQuitOnLastWindowClosed: true},
	})
	service.setEmitter(func(snapshot domain.Snapshot) {
		app.Event.Emit("snapshot", snapshot)
	})

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             "main",
		Title:            "WhiskerLink",
		Width:            1180,
		Height:           760,
		MinWidth:         920,
		MinHeight:        620,
		BackgroundColour: application.NewRGB(246, 247, 242),
		URL:              "/",
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 48,
			Backdrop:                application.MacBackdropTranslucent,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
	})
	window.RegisterHook(events.Common.WindowClosing, func(event *application.WindowEvent) {
		window.Hide()
		event.Cancel()
	})

	tray := app.SystemTray.New()
	tray.SetIcon(appIcon)
	tray.SetTooltip("WhiskerLink")
	tray.OnClick(func() {
		window.Show()
		window.Focus()
	})
	menu := app.NewMenu()
	menu.Add("Open WhiskerLink").OnClick(func(*application.Context) {
		window.Show()
		window.Focus()
	})
	menu.AddSeparator()
	menu.Add("Quit").OnClick(func(*application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)

	if err := app.Run(); err != nil {
		service.shutdown()
		log.Fatal(err)
	}
	service.shutdown()
}
