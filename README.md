# transfer
Simple LAN transfer tool

Use `--interface` to choose the source network interface for outgoing download,
upload, proxy, and relay connections. Pass an interface name (such as `Ethernet`)
or a local IP address (such as `192.168.1.10`). For example:

```sh
transfer --interface Ethernet https://example.com/file
transfer --interface 192.168.1.10 -m upload -c http://192.168.1.20:8080/uploadFile file.txt
```

For segmented downloads, use `--interfaces` with a comma-separated list of
interfaces or local IP addresses. The initial `-x` ranges are distributed in
rotation, with large files starting in 16 MiB chunks. As traffic arrives, idle
workers take more chunks or move unfinished tails toward the interface with the
shorter estimated completion time:

```sh
transfer --interfaces 192.168.233.136,192.168.233.137 -x 16 https://example.com/file
```

For files at least 512 MiB, the two-interface mode probes each route separately,
starts with fewer connections on a clearly slower route, and tests whether adding
connections improves total throughput. Disable this with
`--auto-interface-workers=false`.

When one route slows the other under heavy concurrency, cap active requests per
source with `--interface-workers`. Counts follow the order in `--interfaces` and
their sum may be less than `-x`:

```sh
transfer --interfaces 192.168.233.136,192.168.233.137 --interface-workers 1,12 -x 24 https://example.com/file
```

Server listening addresses are configured separately with `--listen`.
