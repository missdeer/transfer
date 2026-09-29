# transfer
Simple LAN transfer tool

Use `--interface` to choose the source network interface for outgoing download,
upload, proxy, and relay connections. Pass an interface name (such as `Ethernet`)
or a local IP address (such as `192.168.1.10`). For example:

```sh
transfer --interface Ethernet https://example.com/file
transfer --interface 192.168.1.10 -m upload -c http://192.168.1.20:8080/uploadFile file.txt
```

Server listening addresses are configured separately with `--listen`.
