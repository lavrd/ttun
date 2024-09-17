const std = @import("std");
const builtin = @import("builtin");
const time = @cImport(@cInclude("time.h"));

// Init scoped logger.
const log = std.log.scoped(.main);

// Init our custom logger handler.
pub const std_options = .{
    .log_level = .debug,
    .logFn = logFn,
};

// 10MB.
const NetworkBufferLength: usize = 1024 * 1024 * 10;

var wait_signal = true;
var wait_signal_mutex = std.Thread.Mutex{};
var wait_signal_cond = std.Thread.Condition{};

const Side = enum {
    Client,
    Server,
};

const Config = struct {
    side: Side,
};

const Address = struct {
    port: u16,
    ipv4: [4]u8,
};

const ProxyRequest = struct {
    data: []u8,
};

const ProxyResponse = struct {
    data: []u8,
};

fn Channel(comptime T: type) type {
    return struct {
        const Self = @This();

        _raw: std.ArrayList(T),
        _mutex: std.Thread.Mutex,
        _is_closed: bool,

        fn Init(allocator: std.mem.Allocator) Self {
            return .{
                ._raw = std.ArrayList(T).init(allocator),
                ._mutex = .{},
                ._is_closed = false,
            };
        }

        fn send(self: *Self, data: T) !void {
            self._mutex.lock();
            defer self._mutex.unlock();
            if (self._is_closed) return error.Closed;
            try self._raw.insert(0, data);
        }

        fn try_receive(
            self: *Self,
        ) !?T {
            self._mutex.lock();
            defer self._mutex.unlock();
            if (self._is_closed) return error.Closed;
            return self._raw.popOrNull();
        }

        fn close(self: *Self) void {
            self._mutex.lock();
            defer self._mutex.unlock();
            self._raw.clearAndFree();
            self._raw.deinit();
            self._is_closed = true;
        }
    };
}

fn handleSignal(
    signal: i32,
    _: *const std.posix.siginfo_t,
    _: ?*anyopaque,
) callconv(.C) void {
    log.debug("os signal received: {d}", .{signal});
    wait_signal_mutex.lock();
    defer wait_signal_mutex.unlock();
    if (!wait_signal) {
        log.err("received signal second time, exit immediately", .{});
        std.process.exit(1);
    }
    wait_signal = false;
    wait_signal_cond.broadcast();
}

pub fn main() !void {
    switch (builtin.os.tag) {
        .macos, .linux => {},
        else => {
            log.err(
                "ttun can only work on Linux or macOS",
                .{},
            );
            return;
        },
    }

    var gpa = std.heap.GeneralPurposeAllocator(.{
        .safety = true,
        .thread_safe = true,
        .verbose_log = true,
    }){};
    const allocator = gpa.allocator();
    defer std.debug.assert(gpa.deinit() == .ok);

    const config = try parseConfig(allocator);
    log.info("starting as a {any}", .{config});

    var req_ch = Channel(ProxyRequest).Init(allocator);
    var res_ch = Channel(ProxyResponse).Init(allocator);

    var connect_proxy_server_thread: ?std.Thread = null;
    var incoming_server_thread: ?std.Thread = null;
    var proxy_server_thread: ?std.Thread = null;
    switch (config.side) {
        Side.Client => {
            connect_proxy_server_thread = try std.Thread.spawn(.{}, connectToProxyServer, .{});
        },
        Side.Server => {
            incoming_server_thread = try std.Thread.spawn(.{}, incomingServer, .{
                @as(u16, 14600),
                @as(*Channel(ProxyRequest), &req_ch),
                @as(*Channel(ProxyResponse), &res_ch),
            });
            proxy_server_thread = try std.Thread.spawn(.{}, proxyServer, .{
                @as(u16, 22000),
                @as(*Channel(ProxyRequest), &req_ch),
                @as(*Channel(ProxyResponse), &res_ch),
            });
        },
    }

    var act = std.posix.Sigaction{
        .handler = .{ .sigaction = handleSignal },
        .mask = std.posix.empty_sigset,
        .flags = (std.posix.SA.SIGINFO),
    };
    var oact: std.posix.Sigaction = undefined;
    try std.posix.sigaction(std.posix.SIG.INT, &act, &oact);
    waitSignalLoop();

    req_ch.close();
    res_ch.close();

    // Waiting for other threads to be stopped.
    if (connect_proxy_server_thread) |thread| thread.join();
    if (incoming_server_thread) |thread| thread.join();
    if (proxy_server_thread) |thread| thread.join();

    log.info("successfully exiting...", .{});
}

fn incomingServer(
    port: u16,
    req_ch: *Channel(ProxyRequest),
    res_ch: *Channel(ProxyResponse),
) !void {
    const incoming_socket = try initSocket();
    defer std.posix.close(incoming_socket);
    try listen(incoming_socket, "0.0.0.0", port);
    log.info("starting incoming tcp server on {any}", .{port});

    while (shouldWait(5)) {
        var from_addr: std.posix.sockaddr = undefined;
        var from_addr_len: std.posix.socklen_t = @sizeOf(std.posix.sockaddr);
        const client_socket = std.posix.accept(
            incoming_socket,
            &from_addr,
            &from_addr_len,
            std.posix.SOCK.NONBLOCK,
        ) catch |err| {
            switch (err) {
                error.WouldBlock => continue,
                else => {
                    log.err("failed to accpent incoming socket connection: {any}", .{err});
                    return;
                },
            }
        };

        _ = try std.Thread.spawn(.{}, handleIncomingConnection, .{
            @as(std.posix.socket_t, client_socket),
            @as(*Channel(ProxyRequest), req_ch),
            @as(*Channel(ProxyResponse), res_ch),
            @as(std.posix.sockaddr, from_addr),
        });
    }

    log.info("incoming tcp server was stopped by os signal", .{});
}

fn handleIncomingConnection(
    client_socket: std.posix.socket_t,
    req_ch: *Channel(ProxyRequest),
    res_ch: *Channel(ProxyResponse),
    from_addr: std.posix.sockaddr,
) !void {
    defer std.posix.close(client_socket);
    const from_addr_p = try parseSockaddr(from_addr);
    log.debug(
        "new connection to incoming server is established: {any}",
        .{from_addr_p},
    );

    var buf: [NetworkBufferLength]u8 = [_]u8{0} ** NetworkBufferLength;
    while (shouldWait(5)) {
        const n = std.posix.read(client_socket, &buf) catch |err| {
            switch (err) {
                error.WouldBlock => {
                    if (try res_ch.try_receive()) |data| {
                        log.debug(
                            "response was received through channel for connection: {any}",
                            .{from_addr_p},
                        );
                        const n = try std.posix.write(client_socket, data.data);
                        log.debug("successfully wrote {d} bytes to client socket", .{n});
                    }
                    continue;
                },
                else => {
                    log.err("failed to read from icoming socket: {any}", .{err});
                    return;
                },
            }
        };
        if (n == 0) {
            log.info("incoming tcp connection was closed", .{});
            break;
        }
        log.debug("was read {d} bytes from incoming connection: {any}", .{ n, from_addr_p });
        try req_ch.send(ProxyRequest{ .data = buf[0..n] });
        log.debug("request was sent through channel for connection: {any}", .{from_addr_p});
    }
}

fn proxyServer(
    port: u16,
    req_ch: *Channel(ProxyRequest),
    res_ch: *Channel(ProxyResponse),
) !void {
    const proxy_socket = try initSocket();
    defer std.posix.close(proxy_socket);
    try listen(proxy_socket, "0.0.0.0", port);
    log.info("starting proxy tcp server on {any}", .{port});

    while (shouldWait(5)) {
        var client_socket: std.posix.socket_t = undefined;
        var from_addr: std.posix.sockaddr = undefined;
        var from_addr_len: std.posix.socklen_t = @sizeOf(std.posix.sockaddr);

        client_socket = std.posix.accept(
            proxy_socket,
            &from_addr,
            &from_addr_len,
            std.posix.SOCK.NONBLOCK,
        ) catch |err| {
            switch (err) {
                error.WouldBlock => continue,
                else => {
                    log.err("failed to accept new client to proxy server: {any}", .{err});
                    return;
                },
            }
        };
        defer std.posix.close(client_socket);
        const from_addr_p = try parseSockaddr(from_addr);
        log.info("new connection to the proxy server is established: {any}", .{from_addr_p});

        var buf: [NetworkBufferLength]u8 = [_]u8{0} ** NetworkBufferLength;
        while (shouldWait(5)) {
            const n = std.posix.read(client_socket, &buf) catch |err| {
                switch (err) {
                    error.WouldBlock => {
                        if (try req_ch.try_receive()) |req| {
                            _ = try std.posix.write(client_socket, req.data);
                        }
                        continue;
                    },
                    else => {
                        log.err("failed to read from client socket: {any}", .{err});
                        return;
                    },
                }
            };
            if (n == 0) {
                log.info("tcp connection closed", .{});
                break;
            }
            log.debug("was read {d} bytes from the proxy client", .{n});
            try res_ch.send(ProxyResponse{ .data = buf[0..n] });
        }
    }

    log.info("tcp server was stopped by os signal", .{});
}

fn connectToProxyServer() !void {
    const proxy_socket = try initSocket();
    defer std.posix.close(proxy_socket);
    try connect(proxy_socket, "172.17.0.3", 22000);
    log.info("connected to the proxy server", .{});

    const target_socket = try initSocket();
    defer std.posix.close(target_socket);
    try connect(target_socket, "172.17.0.2", 44000);
    log.info("connected to the target server", .{});

    var hp: std.http.HeadParser = .{};
    var buf: [NetworkBufferLength]u8 = [_]u8{0} ** NetworkBufferLength;
    while (shouldWait(5)) {
        const proxy_n = std.posix.read(proxy_socket, &buf) catch |proxy_err| {
            switch (proxy_err) {
                error.WouldBlock => {
                    const target_n = std.posix.read(target_socket, &buf) catch |err| {
                        switch (err) {
                            error.WouldBlock => continue,
                            else => {
                                log.err("failed to read from target socket: {any}", .{err});
                                return;
                            },
                        }
                    };
                    if (target_n == 0) {
                        log.info("tcp connection with target socket was closed", .{});
                        return;
                    }
                    log.debug("read {d} bytes from target connection", .{target_n});

                    if (hp.state != std.http.HeadParser.State.finished) {
                        const header_n = hp.feed(buf[0..target_n]);
                        var header_iter = std.http.HeaderIterator.init(buf[0..header_n]);
                        while (header_iter.next()) |header| {
                            log.debug("Header: {s} = {s}", .{ header.name, header.value });
                        }
                    }

                    while (shouldWait(5)) {
                        const n = std.posix.write(proxy_socket, buf[0..target_n]) catch |err| {
                            switch (err) {
                                error.WouldBlock => {
                                    log.warn("would block on trying to write to proxy socket", .{});
                                    continue;
                                },
                                else => {
                                    log.err("failed to write to proxy socket: {any}", .{err});
                                    return err;
                                },
                            }
                        };
                        log.debug("{d} wrote to the proxy socket", .{n});
                        break;
                    }
                    continue;
                },
                else => {
                    log.err("failed to read from proxy server socket: {any}", .{proxy_err});
                    return;
                },
            }
        };
        if (proxy_n == 0) {
            log.info("tcp connection with proxy server was closed", .{});
            return;
        }
        log.debug("read {d} bytes from proxy server", .{proxy_n});
        _ = try std.posix.write(target_socket, buf[0..proxy_n]);
    }
}

fn waitSignalLoop() void {
    log.info("starting to wait for os signal", .{});
    _ = shouldWait(0);
    log.info("exiting os signal waiting loop", .{});
}

fn shouldWait(ms: u64) bool {
    wait_signal_mutex.lock();
    defer wait_signal_mutex.unlock();
    if (ms == 0) {
        wait_signal_cond.wait(&wait_signal_mutex);
    } else {
        wait_signal_cond.timedWait(
            &wait_signal_mutex,
            ms * std.time.ns_per_ms,
        ) catch |err| switch (err) {
            error.Timeout => {},
        };
    }
    return wait_signal;
}

fn parseConfig(allocator: std.mem.Allocator) !Config {
    var args = try std.process.ArgIterator.initWithAllocator(allocator);
    defer args.deinit();
    // Skip executable
    _ = args.next();
    if (args.next()) |arg| {
        const config = try std.json.parseFromSlice(
            Config,
            allocator,
            arg,
            .{},
        );
        defer config.deinit();
        return config.value;
    }
    return error.NoArgWithConfig;
}

fn initSocket() !std.posix.socket_t {
    // We do not use std.net because client cannot be non-blocking.
    // That's why we are using raw socket.
    // Create and open the TCP socket.
    const socket = try std.posix.socket(std.posix.AF.INET, std.posix.SOCK.STREAM, 0);
    // Make socket work in non-blocking mode.
    const flags = try std.posix.fcntl(socket, std.posix.F.GETFL, 0);
    _ = try std.posix.fcntl(socket, std.posix.F.SETFL, flags | std.posix.SOCK.NONBLOCK);
    // Make socket keep-alive.
    try std.posix.setsockopt(socket, std.posix.SOL.SOCKET, std.posix.SO.KEEPALIVE, &std.mem.toBytes(@as(c_int, 1)));
    return socket;
}

fn listen(socket: std.posix.socket_t, raw_address: []const u8, port: u16) !void {
    // Set socket option to reuse address and port on bind and listen.
    try std.posix.setsockopt(
        socket,
        std.posix.SOL.SOCKET,
        std.posix.SO.REUSEADDR | std.posix.SO.REUSEPORT,
        &std.mem.toBytes(@as(c_int, 1)),
    );
    const address = try std.net.Address.parseIp4(raw_address, port);
    // Bind to address to be able to start listening on it.
    try std.posix.bind(socket, &address.any, address.getOsSockLen());
    // Lister for incoming connections to the socket with buffer.
    try std.posix.listen(socket, 1);
}

fn connect(socket: std.posix.socket_t, raw_address: []const u8, port: u16) !void {
    // Parse address and port to std.net format.
    const address = try std.net.Address.parseIp4(raw_address, port);
    // Connect to the address using socket parameter.
    std.posix.connect(socket, &address.any, address.getOsSockLen()) catch |err| {
        switch (err) {
            error.WouldBlock => log.debug(
                "would block on establishing connection to {any}",
                .{address},
            ),
            else => {
                log.err(
                    "failed to connect to socket: {any}: {any}",
                    .{ address, err },
                );
                return;
            },
        }
    };
}

fn parseSockaddr(sa: std.os.linux.sockaddr) !Address {
    switch (sa.family) {
        std.posix.AF.INET => {
            const port = std.mem.readInt(u16, sa.data[0..2], .little);
            return Address{ .port = port, .ipv4 = [4]u8{
                sa.data[2],
                sa.data[3],
                sa.data[4],
                sa.data[5],
            } };
        },
        else => return error.UnknownFamily,
    }
}

fn logFn(
    comptime level: std.log.Level,
    comptime scope: @TypeOf(.EnumLiteral),
    comptime format: []const u8,
    args: anytype,
) void {
    std.debug.lockStdErr();
    defer std.debug.unlockStdErr();
    const stderr = std.io.getStdErr().writer();

    var buf: [20]u8 = undefined;
    var fba = std.heap.FixedBufferAllocator.init(&buf);
    const allocator = fba.allocator();
    const time_str: []u8 = allocator.alloc(u8, buf.len) catch {
        nosuspend stderr.print(
            "failed to allocate memory to convert timestamp to string\n",
            .{},
        ) catch return;
        return;
    };
    defer allocator.free(time_str);

    const timestamp = time.time(null);
    if (timestamp == -1) {
        nosuspend stderr.print(
            "failed to retrieve current time from time.h\n",
            .{},
        ) catch return;
        return;
    }
    const tm_info = time.localtime(&timestamp);
    const n = time.strftime(
        time_str[0..buf.len],
        buf.len,
        "%Y-%m-%d %H:%M:%S",
        tm_info,
    );
    // We need to compare with buf length - 1 because returning length
    // doesn't contain terminating null character.
    if (n != buf.len - 1) {
        nosuspend stderr.print(
            "failed to format current timestamp using time.h: {d}\n",
            .{n},
        ) catch return;
        return;
    }

    const scoped_level = comptime switch (scope) {
        .gpa => std.log.Level.debug,
        else => level,
    };
    nosuspend stderr.print(
        "{s} " ++ "[" ++ comptime scoped_level.asText() ++ "] " ++ "(" ++ @tagName(scope) ++ ") " ++ format ++ "\n",
        .{time_str} ++ args,
    ) catch return;
}
