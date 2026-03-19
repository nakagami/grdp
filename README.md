# Golang Remote Desktop Protocol

grdp is a pure Golang implementation of the Microsoft RDP (Remote Desktop Protocol) protocol client

Forked from [tomatome/grdp](https://github.com/tomatome/grdp)

## How to execute example

Prepare environment variables
(In your environment. You may also need to set GRDP_DOMAIN)
```
export GRDP_USER=user
export GRDP_PASSWORD=password
export GRDP_PORT=3389
export GRDP_HOST=host
export GRDP_WINDOW_SIZE=1280x800
```

### Environment Variables

| Variable              | Description                              | Default            |
|-----------------------|------------------------------------------|--------------------|
| `GRDP_HOST`           | Hostname or IP address of the RDP server | (required)         |
| `GRDP_PORT`           | Port number                              | (required)         |
| `GRDP_USER`           | Username                                 | (empty)            |
| `GRDP_PASSWORD`       | Password                                 | (empty)            |
| `GRDP_DOMAIN`         | Domain                                   | (empty)            |
| `GRDP_WINDOW_SIZE`    | Window size in `WxH` format              | `1280x800`         |
| `GRDP_KEYBOARD_TYPE`  | Keyboard type (see values below)         | `IBM_101_102_KEYS` |
| `GRDP_KEYBOARD_LAYOUT`| Keyboard layout (see values below)       | `US`               |

#### `GRDP_KEYBOARD_TYPE` values

| Value              | Description                            |
|--------------------|----------------------------------------|
| `IBM_PC_XT_83_KEY` | IBM PC/XT 83-key keyboard              |
| `OLIVETTI`         | Olivetti keyboard                      |
| `IBM_PC_AT_84_KEY` | IBM PC/AT 84-key keyboard              |
| `IBM_101_102_KEYS` | IBM 101/102-key keyboard (most common) |
| `NOKIA_1050`       | Nokia 1050 keyboard                    |
| `NOKIA_9140`       | Nokia 9140 keyboard                    |
| `JAPANESE`         | Japanese keyboard                      |

#### `GRDP_KEYBOARD_LAYOUT` values

| Value                 | Language / Region       |
|-----------------------|-------------------------|
| `ARABIC`              | Arabic                  |
| `BULGARIAN`           | Bulgarian               |
| `CHINESE_US_KEYBOARD` | Chinese (US keyboard)   |
| `CZECH`               | Czech                   |
| `DANISH`              | Danish                  |
| `GERMAN`              | German                  |
| `GREEK`               | Greek                   |
| `US`                  | English (United States) |
| `SPANISH`             | Spanish                 |
| `FINNISH`             | Finnish                 |
| `FRENCH`              | French                  |
| `HEBREW`              | Hebrew                  |
| `HUNGARIAN`           | Hungarian               |
| `ICELANDIC`           | Icelandic               |
| `ITALIAN`             | Italian                 |
| `JAPANESE`            | Japanese                |
| `KOREAN`              | Korean                  |
| `DUTCH`               | Dutch                   |
| `NORWEGIAN`           | Norwegian               |

Clone and execute example
```
git clone https://github.com/nakagami/grdp
cd grdp
go run example/gxui.go
```

This example uses gxui.
Since gxui is no longer being updated, I hope to have some kind of cross-platform GUI example.

See also https://github.com/nakagami/grdpsdl2

## Take ideas from

* [rdpy](https://github.com/citronneur/rdpy)
* [node-rdpjs](https://github.com/citronneur/node-rdpjs)
* [gordp](https://github.com/Madnikulin50/gordp)
* [ncrack_rdp](https://github.com/nmap/ncrack/blob/master/modules/ncrack_rdp.cc)
* [webRDP](https://github.com/Chorder/webRDP)
