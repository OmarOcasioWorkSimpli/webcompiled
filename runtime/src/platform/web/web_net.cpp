// [web] Virtual sockets over the gateway WebSocket; see web_net.h and netgw/PROTOCOL.md.
#include "web_net.h"

#include <arpa/inet.h>
#include <netinet/in.h>

#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <unordered_map>
#include <vector>

extern "C" {
// web/js/library_mkw_net.js
int mkw_net_open();
int mkw_net_alloc(int kind);  // 0 tcp, 1 udp, 2 dns request
void mkw_net_free(int id);
int mkw_net_send(int type, int id, const void* data, int len);
int mkw_net_status(int id, int what);
int mkw_net_recv(int id, void* dst, int cap, int flags, uint32_t* info);
int mkw_net_hello(int idx, char* dst, int cap);
}

namespace WebNet {
namespace {

enum Frame : int {
    DNS_REQ = 0x10, TCP_OPEN = 0x20, TCP_DATA = 0x22, TCP_CLOSE = 0x23,
    UDP_BIND = 0x30, UDP_SEND = 0x32, UDP_CLOSE = 0x34,
};
enum Status : int { kOpened = 0, kClosed = 1, kBound = 2, kDns = 3, kDnsIp = 4, kRxLen = 5 };

constexpr size_t kTcpChunk = 16 * 1024;  // PROTOCOL.md: TCP chunks up to 16 KiB
constexpr size_t kMaxUdp = 2048;

struct VSock {
    int type = SOCK_STREAM;
    enum { Fresh, Connecting, Connected, Failed } state = Fresh;
    int err = 0;                 // errno behind Failed
    bool peerClosed = false;     // we sent TCP_CLOSE (shutdown) - no more writes
    bool bound = false;          // UDP: UDP_BIND sent
    uint16_t localPort = 0;
    bool hasPeer = false;        // TCP connected peer / UDP connect() default destination
    sockaddr_in peer{};
};

std::unordered_map<int, VSock> g_socks;
uint16_t g_nextPort = 40000;  // ponytail: per-client ephemeral counter, wraps without a collision check

uint16_t NextPort() {
    const uint16_t port = g_nextPort++;
    if (g_nextPort < 40000) g_nextPort = 40000;
    return port;
}

VSock* Find(int fd) {
    const auto it = g_socks.find(fd);
    if (it != g_socks.end()) return &it->second;
    errno = EBADF;
    return nullptr;
}

int ErrnoForOpenStatus(int status) {
    switch (status) {
    case 113: return EHOSTUNREACH;
    case 110: return ENETUNREACH;  // Dolphin reports a connect timeout as ENETUNREACH too
    default: return ECONNREFUSED;  // 111 and anything unknown
    }
}

// Settle a pending connect from what the gateway has reported so far.
void Refresh(int fd, VSock& s) {
    if (s.type != SOCK_STREAM || s.state != VSock::Connecting) return;
    const int opened = mkw_net_status(fd, kOpened);
    if (opened == 0) {
        s.state = VSock::Connected;
    } else if (opened > 0) {
        s.state = VSock::Failed;
        s.err = ErrnoForOpenStatus(opened);
    } else if (mkw_net_status(fd, kClosed)) {  // gateway went away before answering
        s.state = VSock::Failed;
        s.err = ECONNRESET;
    }
}

void PutLe16(std::vector<uint8_t>& out, uint16_t v) {
    out.push_back(static_cast<uint8_t>(v));
    out.push_back(static_cast<uint8_t>(v >> 8));
}

bool BindUdp(int fd, VSock& s, uint16_t port) {
    if (!port) port = NextPort();
    std::vector<uint8_t> payload;
    PutLe16(payload, port);
    if (!mkw_net_send(UDP_BIND, fd, payload.data(), static_cast<int>(payload.size()))) {
        errno = ENETDOWN;
        return false;
    }
    s.bound = true;
    s.localPort = port;
    return true;
}

}  // namespace

const Hello& GetHello() {
    static Hello hello;
    if (!hello.ready && mkw_net_open()) {
        char text[64];
        const auto field = [&](int idx) { return mkw_net_hello(idx, text, sizeof(text)) ? std::string(text) : std::string(); };
        in_addr addr{};
        if (inet_pton(AF_INET, field(0).c_str(), &addr) == 1) hello.vip = ntohl(addr.s_addr);
        if (inet_pton(AF_INET, field(1).c_str(), &addr) == 1) hello.gw = ntohl(addr.s_addr);
        hello.userId = std::strtoull(field(2).c_str(), nullptr, 10);
        hello.serno = field(3);
        hello.mac = field(4);
        hello.ready = true;
    }
    return hello;
}

bool Online() { return GetHello().ready; }

int DnsStart(const std::string& name) {
    const int id = mkw_net_alloc(2);
    if (!id) return 0;
    if (!mkw_net_send(DNS_REQ, id, name.data(), static_cast<int>(name.size()))) {
        mkw_net_free(id);
        return 0;
    }
    return id;
}

int DnsPoll(int id, uint32_t* ipHostOrder) {
    const int state = mkw_net_status(id, kDns);
    if (state < 0) return -1;
    *ipHostOrder = static_cast<uint32_t>(mkw_net_status(id, kDnsIp));
    mkw_net_free(id);
    return state;
}

void DnsCancel(int id) { mkw_net_free(id); }

int socket(int af, int type, int) {
    if (af != AF_INET) { errno = EAFNOSUPPORT; return -1; }
    if (type != SOCK_STREAM && type != SOCK_DGRAM) { errno = EPROTONOSUPPORT; return -1; }
    const int fd = mkw_net_alloc(type == SOCK_DGRAM ? 1 : 0);
    if (!fd) { errno = ENETDOWN; return -1; }
    g_socks[fd].type = type;
    return fd;
}

int connect(int fd, const sockaddr* addr, socklen_t len) {
    VSock* s = Find(fd);
    if (!s) return -1;
    if (!addr || len < sizeof(sockaddr_in)) { errno = EINVAL; return -1; }
    const auto& in = *reinterpret_cast<const sockaddr_in*>(addr);
    if (s->type == SOCK_DGRAM) {  // only sets the default destination
        s->peer = in;
        s->hasPeer = true;
        return 0;
    }
    Refresh(fd, *s);
    switch (s->state) {
    case VSock::Connected: errno = EISCONN; return -1;
    case VSock::Connecting: errno = EALREADY; return -1;
    case VSock::Failed: errno = s->err; return -1;
    case VSock::Fresh: break;
    }
    std::vector<uint8_t> payload(reinterpret_cast<const uint8_t*>(&in.sin_addr.s_addr),
                                 reinterpret_cast<const uint8_t*>(&in.sin_addr.s_addr) + 4);
    PutLe16(payload, ntohs(in.sin_port));
    if (!mkw_net_send(TCP_OPEN, fd, payload.data(), static_cast<int>(payload.size()))) {
        errno = ENETUNREACH;
        return -1;
    }
    s->peer = in;
    s->hasPeer = true;
    s->state = VSock::Connecting;
    if (!s->localPort) s->localPort = NextPort();
    errno = EINPROGRESS;  // always non-blocking: completion is observed through poll()/SO_ERROR
    return -1;
}

int bind(int fd, const sockaddr* addr, socklen_t len) {
    VSock* s = Find(fd);
    if (!s) return -1;
    if (!addr || len < sizeof(sockaddr_in)) { errno = EINVAL; return -1; }
    const uint16_t port = ntohs(reinterpret_cast<const sockaddr_in*>(addr)->sin_port);
    if (s->type == SOCK_DGRAM) {
        if (s->bound) { errno = EINVAL; return -1; }
        return BindUdp(fd, *s, port) ? 0 : -1;
    }
    s->localPort = port;
    return 0;
}

ssize_t sendto(int fd, const void* data, size_t size, int, const sockaddr* dest, socklen_t destLen) {
    VSock* s = Find(fd);
    if (!s) return -1;
    const auto* bytes = static_cast<const uint8_t*>(data);
    if (s->type == SOCK_STREAM) {
        Refresh(fd, *s);
        if (s->state == VSock::Failed) { errno = s->err; return -1; }
        if (s->state != VSock::Connected) { errno = ENOTCONN; return -1; }
        if (s->peerClosed || mkw_net_status(fd, kClosed)) { errno = EPIPE; return -1; }
        // ponytail: no WebSocket bufferedAmount backpressure; the game's traffic is small.
        for (size_t off = 0; off < size; off += kTcpChunk) {
            const int n = static_cast<int>(std::min(kTcpChunk, size - off));
            if (!mkw_net_send(TCP_DATA, fd, bytes + off, n)) { errno = ECONNRESET; return -1; }
        }
        return static_cast<ssize_t>(size);
    }
    sockaddr_in to{};
    if (dest && destLen >= sizeof(sockaddr_in)) to = *reinterpret_cast<const sockaddr_in*>(dest);
    else if (s->hasPeer) to = s->peer;
    else { errno = EDESTADDRREQ; return -1; }
    if (size > kMaxUdp) { errno = EMSGSIZE; return -1; }
    if (!s->bound && !BindUdp(fd, *s, 0)) return -1;
    std::vector<uint8_t> payload(reinterpret_cast<const uint8_t*>(&to.sin_addr.s_addr),
                                 reinterpret_cast<const uint8_t*>(&to.sin_addr.s_addr) + 4);
    PutLe16(payload, ntohs(to.sin_port));
    payload.insert(payload.end(), bytes, bytes + size);
    if (!mkw_net_send(UDP_SEND, fd, payload.data(), static_cast<int>(payload.size()))) { errno = ENETDOWN; return -1; }
    return static_cast<ssize_t>(size);
}

ssize_t recvfrom(int fd, void* data, size_t size, int flags, sockaddr* from, socklen_t* fromLen) {
    VSock* s = Find(fd);
    if (!s) return -1;
    Refresh(fd, *s);
    uint32_t info[2] = {0, 0};
    const int n = mkw_net_recv(fd, data, static_cast<int>(size), (flags & MSG_PEEK) ? 1 : 0, info);
    if (n >= 0) {
        if (from && fromLen && *fromLen >= sizeof(sockaddr_in)) {
            sockaddr_in src{};
            src.sin_family = AF_INET;
            if (s->type == SOCK_DGRAM) {
                src.sin_addr.s_addr = htonl(info[0]);
                src.sin_port = htons(static_cast<uint16_t>(info[1]));
            } else if (s->hasPeer) {
                src = s->peer;
            }
            std::memcpy(from, &src, sizeof(src));
            *fromLen = sizeof(src);
        }
        return n;
    }
    if (s->type == SOCK_DGRAM) { errno = EAGAIN; return -1; }
    if (s->state == VSock::Failed) { errno = s->err; return -1; }
    if (s->state != VSock::Connected) { errno = ENOTCONN; return -1; }
    if (mkw_net_status(fd, kClosed)) return 0;  // peer closed and everything was read: EOF
    errno = EAGAIN;
    return -1;
}

int getsockname(int fd, sockaddr* addr, socklen_t* len) {
    VSock* s = Find(fd);
    if (!s) return -1;
    sockaddr_in out{};
    out.sin_family = AF_INET;
    out.sin_addr.s_addr = htonl(GetHello().vip);
    out.sin_port = htons(s->localPort);
    const socklen_t n = std::min<socklen_t>(*len, sizeof(out));
    std::memcpy(addr, &out, n);
    *len = sizeof(out);
    return 0;
}

int getpeername(int fd, sockaddr* addr, socklen_t* len) {
    VSock* s = Find(fd);
    if (!s) return -1;
    Refresh(fd, *s);
    if (!s->hasPeer || (s->type == SOCK_STREAM && s->state != VSock::Connected)) { errno = ENOTCONN; return -1; }
    const socklen_t n = std::min<socklen_t>(*len, sizeof(s->peer));
    std::memcpy(addr, &s->peer, n);
    *len = sizeof(s->peer);
    return 0;
}

int getsockopt(int fd, int level, int name, void* value, socklen_t* len) {
    VSock* s = Find(fd);
    if (!s) return -1;
    Refresh(fd, *s);
    int v = 0;
    if (level == SOL_SOCKET) {
        if (name == SO_ERROR) v = s->state == VSock::Failed ? s->err : 0;
        else if (name == SO_TYPE) v = s->type;
    }
    const socklen_t n = std::min<socklen_t>(*len, sizeof(v));
    std::memcpy(value, &v, n);
    *len = n;
    return 0;
}

int setsockopt(int fd, int, int, const void*, socklen_t) { return Find(fd) ? 0 : -1; }

int listen(int, int) { errno = EOPNOTSUPP; return -1; }

int shutdown(int fd, int how) {
    VSock* s = Find(fd);
    if (!s) return -1;
    if (s->type == SOCK_STREAM && s->state == VSock::Connected && how != SHUT_RD && !s->peerClosed) {
        mkw_net_send(TCP_CLOSE, fd, nullptr, 0);  // the gateway has no half-close: this closes both ways
        s->peerClosed = true;
    } else if (s->state != VSock::Connected && s->type == SOCK_STREAM) {
        errno = ENOTCONN;
        return -1;
    }
    return 0;
}

int close(int fd) {
    const auto it = g_socks.find(fd);
    if (it == g_socks.end()) return 0;
    const VSock& s = it->second;
    if (s.type == SOCK_STREAM && s.state != VSock::Fresh && !s.peerClosed) mkw_net_send(TCP_CLOSE, fd, nullptr, 0);
    if (s.type == SOCK_DGRAM && s.bound) mkw_net_send(UDP_CLOSE, fd, nullptr, 0);
    mkw_net_free(fd);
    g_socks.erase(it);
    return 0;
}

int Poll(pollfd* fds, size_t count) {
    int ready = 0;
    for (size_t i = 0; i < count; ++i) {
        pollfd& p = fds[i];
        p.revents = 0;
        const auto it = g_socks.find(p.fd);
        if (it == g_socks.end()) { p.revents = POLLNVAL; ++ready; continue; }
        VSock& s = it->second;
        Refresh(p.fd, s);
        bool readable;
        bool writable;
        int extra = 0;
        if (s.type == SOCK_DGRAM) {
            readable = mkw_net_status(p.fd, kRxLen) >= 0;
            writable = true;
        } else {
            const bool closed = mkw_net_status(p.fd, kClosed) != 0;
            readable = mkw_net_status(p.fd, kRxLen) >= 0 || closed || s.state == VSock::Failed;
            writable = s.state == VSock::Connected || s.state == VSock::Failed;
            if (s.state == VSock::Failed) extra = POLLERR;
            else if (closed && s.state == VSock::Connected) extra = POLLHUP;
        }
        if (readable) p.revents |= p.events & (POLLIN | POLLRDNORM);
        if (writable) p.revents |= p.events & (POLLOUT | POLLWRNORM);
        p.revents |= extra;
        if (p.revents) ++ready;
    }
    return ready;
}

}  // namespace WebNet
