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

        _raw: ?T,
        _mutex: std.Thread.Mutex,
        _cond: std.Thread.Condition,

        fn Init(value: ?T) Self {
            return .{
                ._raw = value,
                ._mutex = .{},
                ._cond = .{},
            };
        }

        fn send(self: *Self, data: T) void {
            self._mutex.lock();
            defer self._mutex.unlock();
            self._raw = data;
            self._cond.signal();
        }

        fn receive(self: *Self) ?T {
            self._mutex.lock();
            defer self._mutex.unlock();
            self._cond.wait(&self._mutex);
            return self._change();
        }

        fn close(self: *Self) void {
            self._mutex.lock();
            defer self._mutex.unlock();
            // Currently channel works like that:
            // - If someone wait using "receive" function
            // - And cond wakes up
            // - And value in null
            // - It means channel is closed.
            self._raw = null;
            self._cond.signal();
        }

        fn _change(self: *Self) ?T {
            if (self._raw) |raw| {
                self._raw = null;
                return raw;
            }
            return null;
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

    var req_ch = Channel(ProxyRequest).Init(null);
    var res_ch = Channel(ProxyResponse).Init(null);

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
    const socket = try initSocket();
    defer std.posix.close(socket);
    try listen(socket, "0.0.0.0", port);
    log.info("starting incoming tcp server on {any}", .{port});

    while (shouldWait(5)) {
        var from_addr: std.posix.sockaddr = undefined;
        var from_addr_len: std.posix.socklen_t = @sizeOf(std.posix.sockaddr);
        const conn = std.posix.accept(socket, &from_addr, &from_addr_len, 0) catch |err| {
            switch (err) {
                error.WouldBlock => continue,
                else => return,
            }
        };
        const from_addr_p = try parseSockaddr(from_addr);
        log.debug("new connection to incoming server: {any}", .{from_addr_p});

        var buf: [4096]u8 = [_]u8{0} ** 4096;
        const n = try std.posix.read(conn, &buf);
        log.debug("was read {d} bytes from connection: {any}", .{ n, from_addr_p });
        req_ch.send(ProxyRequest{ .data = buf[0..n] });
        log.debug("request was sent through channel for connection: {any}", .{from_addr_p});
        if (res_ch.receive()) |data| {
            log.debug("response was received through channel for connection: {any}", .{from_addr_p});
            _ = try std.posix.write(conn, data.data);
        }
        std.posix.close(conn);
    }

    log.info("incoming tcp server was stopped by os signal", .{});
}

fn proxyServer(
    port: u16,
    req_ch: *Channel(ProxyRequest),
    res_ch: *Channel(ProxyResponse),
) !void {
    const socket = try initSocket();
    defer std.posix.close(socket);
    try listen(socket, "0.0.0.0", port);
    log.info("starting proxy tcp server on {any}", .{port});

    wait_connection: while (shouldWait(5)) {
        var conn: std.posix.socket_t = undefined;
        var from_addr: std.posix.sockaddr = undefined;
        var from_addr_len: std.posix.socklen_t = @sizeOf(std.posix.sockaddr);
        while (shouldWait(5)) {
            conn = std.posix.accept(socket, &from_addr, &from_addr_len, 0) catch |err| {
                switch (err) {
                    error.WouldBlock => continue,
                    else => return,
                }
            };
            break;
        } else {
            log.info("tcp server was stopped by os signal", .{});
            return;
        }
        defer std.posix.close(conn);
        log.info("new connection is established: {any}", .{from_addr});

        wait_data: while (shouldWait(5)) {
            const request = req_ch.receive();
            if (request) |req| {
                _ = try std.posix.write(conn, req.data);
                var buf: [1024]u8 = [_]u8{0} ** 1024;
                while (shouldWait(5)) {
                    const n = std.posix.read(conn, &buf) catch |err| {
                        switch (err) {
                            error.WouldBlock => continue,
                            else => return,
                        }
                    };
                    if (n == 0) {
                        log.info("tcp connection closed", .{});
                        continue :wait_connection;
                    }
                    res_ch.send(ProxyResponse{ .data = buf[0..n] });
                    continue :wait_data;
                }
            }
            // It means channels were closed.
            continue :wait_connection;
        }
    }

    log.info("tcp server was stopped by os signal", .{});
}

fn connectToProxyServer() !void {
    const socket = try initSocket();
    defer std.posix.close(socket);
    // Connect to the proxy server.
    try connect(socket, "172.17.0.3", 22000);
    log.info("connected to the proxy server", .{});

    var buf: [1024]u8 = [_]u8{0} ** 1024;
    while (shouldWait(5)) {
        const n = std.posix.read(socket, &buf) catch |err| {
            switch (err) {
                error.WouldBlock => continue,
                else => {
                    log.err("failed to read from proxy server: {any}", .{err});
                    return;
                },
            }
        };
        log.debug("read {d} bytes from proxy server", .{n});

        const target_socket = try initSocket();
        defer std.posix.close(target_socket);
        try connect(target_socket, "172.17.0.2", 44000);

        _ = try std.posix.write(target_socket, buf[0..n]);
        while (shouldWait(5)) {
            const target_n = std.posix.read(target_socket, &buf) catch |err| {
                switch (err) {
                    error.WouldBlock => continue,
                    else => return,
                }
            };
            _ = try std.posix.write(socket, buf[0..target_n]);
            // We need to exit from this loop as we already read and send data.
            break;
        }
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
    // Create and open the socket.
    const socket = try std.posix.socket(std.posix.AF.INET, std.posix.SOCK.STREAM, 0);
    // Make socket work in non-blocking mode.
    const flags = try std.posix.fcntl(socket, std.posix.F.GETFL, 0);
    _ = try std.posix.fcntl(socket, std.posix.F.SETFL, flags | std.posix.SOCK.NONBLOCK);
    return socket;
}

fn listen(socket: std.posix.socket_t, raw_address: []const u8, port: u16) !void {
    // Reuse address and port.
    try std.posix.setsockopt(
        socket,
        std.posix.SOL.SOCKET,
        std.posix.SO.REUSEADDR | std.posix.SO.REUSEPORT,
        &std.mem.toBytes(@as(c_int, 1)),
    );
    // Bind to address to be able to start listening on it.
    const address = try std.net.Address.parseIp4(raw_address, port);
    try std.posix.bind(socket, &address.any, address.getOsSockLen());
    try std.posix.listen(socket, 1);
}

fn connect(socket: std.posix.socket_t, raw_address: []const u8, port: u16) !void {
    const address = try std.net.Address.parseIp4(raw_address, port);
    std.posix.connect(socket, &address.any, address.getOsSockLen()) catch |err| {
        switch (err) {
            error.WouldBlock => log.debug("would block on connection to {any}", .{address}),
            else => {
                log.err("failed to connect to proxy server: {any}", .{err});
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
