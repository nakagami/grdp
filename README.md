# Golang Remote Desktop Protocol

grdp is a pure Golang implementation of the Microsoft RDP (Remote Desktop Protocol) protocol client

Forked from [tomatome/grdp](https://github.com/tomatome/grdp)

## Unsupported feature

Remove VNC client feature.

Remove features that work only on certain platforms.
For example clipboard.

## TODO

- Ubuntu RDP server support
- RDP client (Currently, there is only an example that demonstrates the library's functionality).

## How to execute example

Prepare environment variables
(In your environment. You may also need to set GRDP_DOMAIN)
```
export GRDP_USER=user
export GRDP_PASSWORD=password
export GRDP_PORT=3389
export GRDP_HOST=host
```

Clone and execute example
```
git clone https://github.com/nakagami/grdp
cd grdp
go run example/gxui.go
```

## Take ideas from

* [rdpy](https://github.com/citronneur/rdpy)
* [node-rdpjs](https://github.com/citronneur/node-rdpjs)
* [gordp](https://github.com/Madnikulin50/gordp)
* [ncrack_rdp](https://github.com/nmap/ncrack/blob/master/modules/ncrack_rdp.cc)
* [webRDP](https://github.com/Chorder/webRDP)
