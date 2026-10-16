/*
 * wolf_loopback.c - preloaded into a session pod's Wolf (XERK-1682).
 *
 * Wolf binds its Moonlight HTTP and HTTPS servers to 0.0.0.0, with no setting
 * to change it, and session pods are host-networked, so both are reachable on
 * the node IP. Its HTTPS client-cert check accepts any cert whose issuer it
 * can't find (a moonlight-embedded workaround), so a cert naming a made-up CA
 * passes as a paired client. Nobody needs either server from outside the pod:
 * Moonlight clients talk to moonlight-proxy, and the operator drives Wolf
 * through wolf-agent and Wolf's unix socket.
 *
 * So a wildcard bind() (0.0.0.0, ::, or ::ffff:0.0.0.0) to WOLF_HTTP_PORT or
 * WOLF_HTTPS_PORT (Wolf's own defaults when unset) is narrowed to loopback.
 * Every other bind is untouched.
 *
 * Loopback alone is not enough (XERK-1737): every host-networked pod on the
 * node shares 127.0.0.1, including other tenants' game containers. So every
 * connection accept() takes on one of those two listeners is closed before
 * Wolf sees it, and accept() goes on to the next; on a non-blocking listener
 * (asio's) it ends with EAGAIN once the queue is drained, which asio treats as
 * "wait for more". The servers stay bound, since Wolf exits if they can't be,
 * but serve nobody. Moving them to an AF_UNIX socket instead would swap the
 * file under an fd asio has already registered with epoll, so accepts would
 * never wake.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <netinet/in.h>
#include <stddef.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>
#include <unistd.h>

/* Wolf's defaults, used when the variable is unset (state::HTTP_PORT etc.). */
#define WOLF_DEFAULT_HTTP_PORT 47989
#define WOLF_DEFAULT_HTTPS_PORT 47984

typedef int (*bind_fn)(int, const struct sockaddr *, socklen_t);
typedef int (*accept4_fn)(int, struct sockaddr *, socklen_t *, int);
typedef int (*accept_fn)(int, struct sockaddr *, socklen_t *);

static long env_port(const char *name, long def) {
  const char *v = getenv(name);
  if (v == NULL || *v == '\0')
    return def;
  /* Wolf parses it with std::stoi (stops at the first non-digit) and keeps
   * it as an unsigned short, so 85537 means 20001. */
  return (unsigned short)strtol(v, NULL, 10);
}

/* Read once at load: accept() runs on Wolf's threads for its whole life, and
 * getenv there could race a setenv elsewhere in Wolf. */
static long wolf_http_port, wolf_https_port;

__attribute__((constructor)) static void read_wolf_ports(void) {
  wolf_http_port = env_port("WOLF_HTTP_PORT", WOLF_DEFAULT_HTTP_PORT);
  wolf_https_port = env_port("WOLF_HTTPS_PORT", WOLF_DEFAULT_HTTPS_PORT);
}

static int is_wolf_rest_port(in_port_t net_port) {
  long port = ntohs(net_port);
  /* Port 0 (an ephemeral bind, or one before read_wolf_ports ran) is never
   * one of Wolf's servers. */
  return port != 0 && (port == wolf_http_port || port == wolf_https_port);
}

int bind(int fd, const struct sockaddr *addr, socklen_t len) {
  static bind_fn real_bind;
  if (real_bind == NULL) {
    real_bind = (bind_fn)dlsym(RTLD_NEXT, "bind");
    if (real_bind == NULL) {
      errno = ENOSYS;
      return -1;
    }
  }

  if (addr != NULL && addr->sa_family == AF_INET && len >= sizeof(struct sockaddr_in)) {
    struct sockaddr_in in4;
    memcpy(&in4, addr, sizeof(in4));
    if (in4.sin_addr.s_addr == htonl(INADDR_ANY) && is_wolf_rest_port(in4.sin_port)) {
      in4.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
      return real_bind(fd, (const struct sockaddr *)&in4, sizeof(in4));
    }
  } else if (addr != NULL && addr->sa_family == AF_INET6 && len >= offsetof(struct sockaddr_in6, sin6_scope_id)) {
    /* Linux also takes the RFC 2133 length (no sin6_scope_id). */
    struct sockaddr_in6 in6;
    memset(&in6, 0, sizeof(in6));
    memcpy(&in6, addr, len < sizeof(in6) ? len : sizeof(in6));
    if (is_wolf_rest_port(in6.sin6_port)) {
      if (IN6_IS_ADDR_UNSPECIFIED(&in6.sin6_addr)) {
        in6.sin6_addr = in6addr_loopback;
        return real_bind(fd, (const struct sockaddr *)&in6, sizeof(in6));
      }
      /* ::ffff:0.0.0.0 is the IPv4 wildcard on a dual-stack socket
       * (XERK-1972): narrow it to ::ffff:127.0.0.1. */
      if (IN6_IS_ADDR_V4MAPPED(&in6.sin6_addr) && in6.sin6_addr.s6_addr32[3] == htonl(INADDR_ANY)) {
        in6.sin6_addr.s6_addr32[3] = htonl(INADDR_LOOPBACK);
        return real_bind(fd, (const struct sockaddr *)&in6, sizeof(in6));
      }
    }
  }
  return real_bind(fd, addr, len);
}

/* is_wolf_rest_listener reports whether fd is bound to Wolf's HTTP or HTTPS
 * port. It asks the socket itself rather than remembering bind()s, so a reused
 * fd number can't be mistaken for one. */
static int is_wolf_rest_listener(int fd) {
  struct sockaddr_storage ss;
  socklen_t len = sizeof(ss);
  if (getsockname(fd, (struct sockaddr *)&ss, &len) != 0)
    return 0;
  if (ss.ss_family == AF_INET && len >= sizeof(struct sockaddr_in))
    return is_wolf_rest_port(((struct sockaddr_in *)&ss)->sin_port);
  if (ss.ss_family == AF_INET6 && len >= sizeof(struct sockaddr_in6))
    return is_wolf_rest_port(((struct sockaddr_in6 *)&ss)->sin6_port);
  return 0;
}

/* accept_dropping accepts on fd through real accept (use_accept4 == 0) or
 * accept4, closing every connection if fd is a Wolf REST listener. It returns
 * the first connection on any other socket, or the error that ends the loop
 * (EAGAIN on a drained non-blocking listener). */
static int accept_dropping(int fd, struct sockaddr *addr, socklen_t *len, int flags, int use_accept4) {
  static accept_fn real_accept;
  static accept4_fn real_accept4;
  if (use_accept4 ? real_accept4 == NULL : real_accept == NULL) {
    if (use_accept4)
      real_accept4 = (accept4_fn)dlsym(RTLD_NEXT, "accept4");
    else
      real_accept = (accept_fn)dlsym(RTLD_NEXT, "accept");
    if (use_accept4 ? real_accept4 == NULL : real_accept == NULL) {
      errno = ENOSYS;
      return -1;
    }
  }

  socklen_t want = len != NULL ? *len : 0;
  int rest = -1; /* unknown until the first connection */
  for (;;) {
    if (len != NULL)
      *len = want;
    int c = use_accept4 ? real_accept4(fd, addr, len, flags) : real_accept(fd, addr, len);
    if (c < 0)
      return c;
    if (rest < 0)
      rest = is_wolf_rest_listener(fd);
    if (!rest)
      return c;
    close(c);
  }
}

int accept(int fd, struct sockaddr *addr, socklen_t *len) {
  return accept_dropping(fd, addr, len, 0, 0);
}

int accept4(int fd, struct sockaddr *addr, socklen_t *len, int flags) {
  return accept_dropping(fd, addr, len, flags, 1);
}
