#pragma once

// [web] Private-network sockets for the browser build. Browsers have no raw sockets, so the guest's
// /dev/net/ip/top sockets are virtual: a POSIX-shaped layer (socket/connect/sendto/recvfrom/poll,
// errno on failure, always non-blocking) over the one gateway WebSocket described in
// netgw/PROTOCOL.md. The WebSocket and the receive queues live in web/site/net.js and are reached
// through the plain synchronous imports of web/js/library_mkw_net.js; nothing here suspends and
// nothing is called back from JS. The hle/net code reaches this through the `Sys` alias (on every
// other platform `Sys` re-exports the host's own socket calls, see hle/net/network_internal.h).

#include <poll.h>
#include <sys/socket.h>

#include <cstddef>
#include <cstdint>
#include <string>

namespace WebNet {

// Identity the gateway assigned in its hello message (derived from the per-browser device id).
struct Hello {
    bool ready = false;                 // false: offline / hello not received (fields below are fallbacks)
    uint32_t vip = 0x0A4D0002;          // 10.77.0.2, host order
    uint32_t gw = 0x0A4D0001;           // 10.77.0.1
    uint64_t userId = 0;
    std::string serno;                  // 9 digits
    std::string mac;                    // "00:09:bf:xx:xx:xx"
};
const Hello& GetHello();                // re-checks the gateway until the hello has been seen
bool Online();                          // GetHello().ready

// Name resolution through the gateway (DNS_REQ). DnsStart returns a request id, or 0 when the gateway
// is down; DnsPoll returns -1 while pending, 0 if the name did not resolve, 1 with *ipHostOrder set.
int DnsStart(const std::string& name);
int DnsPoll(int id, uint32_t* ipHostOrder);
void DnsCancel(int id);

int socket(int af, int type, int protocol);
int connect(int fd, const sockaddr* addr, socklen_t len);
int bind(int fd, const sockaddr* addr, socklen_t len);
ssize_t sendto(int fd, const void* data, size_t size, int flags, const sockaddr* dest, socklen_t destLen);
ssize_t recvfrom(int fd, void* data, size_t size, int flags, sockaddr* from, socklen_t* fromLen);
inline ssize_t send(int fd, const void* data, size_t size, int flags) { return sendto(fd, data, size, flags, nullptr, 0); }
inline ssize_t recv(int fd, void* data, size_t size, int flags) { return recvfrom(fd, data, size, flags, nullptr, nullptr); }
int getsockname(int fd, sockaddr* addr, socklen_t* len);
int getpeername(int fd, sockaddr* addr, socklen_t* len);
int getsockopt(int fd, int level, int name, void* value, socklen_t* len);
int setsockopt(int fd, int level, int name, const void* value, socklen_t len);
int listen(int fd, int backlog);
int shutdown(int fd, int how);
int close(int fd);
int Poll(pollfd* fds, size_t count);    // zero-timeout readiness probe, like poll(fds, n, 0)

}  // namespace WebNet

namespace Sys = WebNet;
