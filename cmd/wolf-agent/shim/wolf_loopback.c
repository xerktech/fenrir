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
 * So a wildcard bind() to WOLF_HTTP_PORT or WOLF_HTTPS_PORT (Wolf's own
 * defaults when unset) is narrowed to loopback. Every other bind is untouched.
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <netinet/in.h>
#include <stdlib.h>
#include <string.h>
#include <sys/socket.h>

/* Wolf's defaults, used when the variable is unset (state::HTTP_PORT etc.). */
#define WOLF_DEFAULT_HTTP_PORT 47989
#define WOLF_DEFAULT_HTTPS_PORT 47984

typedef int (*bind_fn)(int, const struct sockaddr *, socklen_t);

static long env_port(const char *name, long def) {
  const char *v = getenv(name);
  if (v == NULL || *v == '\0')
    return def;
  /* Wolf parses it with std::stoi, which stops at the first non-digit. */
  return strtol(v, NULL, 10);
}

static int is_wolf_rest_port(in_port_t net_port) {
  long port = ntohs(net_port);
  return port == env_port("WOLF_HTTP_PORT", WOLF_DEFAULT_HTTP_PORT) ||
         port == env_port("WOLF_HTTPS_PORT", WOLF_DEFAULT_HTTPS_PORT);
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
  } else if (addr != NULL && addr->sa_family == AF_INET6 && len >= sizeof(struct sockaddr_in6)) {
    struct sockaddr_in6 in6;
    memcpy(&in6, addr, sizeof(in6));
    if (IN6_IS_ADDR_UNSPECIFIED(&in6.sin6_addr) && is_wolf_rest_port(in6.sin6_port)) {
      in6.sin6_addr = in6addr_loopback;
      return real_bind(fd, (const struct sockaddr *)&in6, sizeof(in6));
    }
  }
  return real_bind(fd, addr, len);
}
