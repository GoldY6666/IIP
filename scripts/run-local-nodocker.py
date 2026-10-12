"""IIP local run - one-shot launcher for the whole stack without Docker.

Why a single script instead of N terminals:
  In sandboxed/automated environments no descendant process survives the tool
  call that spawned it. So PostgreSQL, iip-server and Vite must all be children
  of THIS script, and the script must stay alive for the stack to stay up.
  In an interactive terminal the same script works fine too - it is just a
  convenient three-in-one launcher.

Stack:
  PostgreSQL  127.0.0.1:5433   (green install, db=iip)
  iip-server  0.0.0.0:8080     (vite proxies /api -> :8080)
  web (vite)  127.0.0.1:5173

Configuration (env vars, all optional):
  IIP_PROJ        project root          default: 2 levels up from this file
  IIP_PG_HOME     PostgreSQL install    default: D:\\pgsql16
  IIP_PG_PORT     PostgreSQL port       default: 5433
  IIP_PG_USER     superuser             default: postgres
  IIP_PG_PASSWORD superuser password    default: dev
  IIP_PG_DB       database name         default: iip
  IIP_HTTP_PORT   server port           default: 8080
  IIP_VITE_PORT   vite dev server port  default: 5173

Progress is written continuously to <log dir>/_run_state.txt so it is
observable from another process while this script keeps running.

Usage:
  python scripts/run-local-nodocker.py
"""
import os
import subprocess
import sys
import time
import urllib.error
import urllib.request

_here = os.path.dirname(os.path.abspath(__file__))
PROJ = os.environ.get("IIP_PROJ", os.path.dirname(_here))
PG_HOME = os.environ.get("IIP_PG_HOME", r"D:\pgsql16")
SETUP = os.environ.get("IIP_LOG_DIR", os.path.join(PG_HOME, "logs"))
os.makedirs(SETUP, exist_ok=True)
STATE = os.path.join(SETUP, "_run_state.txt")

PG_PORT = int(os.environ.get("IIP_PG_PORT", "5433"))
HTTP_PORT = int(os.environ.get("IIP_HTTP_PORT", "8080"))
VITE_PORT = int(os.environ.get("IIP_VITE_PORT", "5173"))
PG_USER = os.environ.get("IIP_PG_USER", "postgres")
STRICT_PORT = os.environ.get("IIP_STRICT_PORT", "1") == "1"
PG_PASSWORD = os.environ.get("IIP_PG_PASSWORD", "dev")
PG_DB = os.environ.get("IIP_PG_DB", "iip")
PGURL = (f"postgres://{PG_USER}:{PG_PASSWORD}@127.0.0.1:{PG_PORT}/{PG_DB}"
         f"?sslmode=disable")

SERVER_EXE = os.path.join(PROJ, "bin", "iip-server.exe")
if not os.path.exists(SERVER_EXE):
    SERVER_EXE = os.path.join(PROJ, "bin", "iip-server")
PSQL = os.path.join(PG_HOME, "bin", "psql.exe")
PG_ISREADY = os.path.join(PG_HOME, "bin", "pg_isready.exe")
POSTGRES = os.path.join(PG_HOME, "bin", "postgres.exe")
PG_LOG = os.path.join(PG_HOME, "pg.log")

# Windows-only process creation flags. On POSIX these evaluate to 0 so the same
# script runs unchanged; the detached-parent trick is a Windows sandbox concern.
IS_WIN = sys.platform == "win32"
DETACHED_PROCESS = 0x00000008 if IS_WIN else 0
CREATE_NEW_PROCESS_GROUP = 0x00000200 if IS_WIN else 0
CREATE_NO_WINDOW = 0x08000000 if IS_WIN else 0

L = []


def w(s=""):
    L.append(str(s))
    try:
        with open(STATE, "w", encoding="utf-8") as fh:
            fh.write("\n".join(L))
    except Exception:  # noqa: BLE001
        pass


def run(args, timeout=60, env=None, cwd=None):
    try:
        r = subprocess.run(args, capture_output=True, text=True, timeout=timeout,
                           encoding="utf-8", errors="replace",
                           env=env or os.environ, cwd=cwd)
        return r.returncode, (r.stdout or "") + (r.stderr or "")
    except Exception as exc:  # noqa: BLE001
        return -998, "EXC %r" % (exc,)


def http_get(url, timeout=4):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, r.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, (e.read() or b"").decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        return None, repr(e)


def port_listening(port):
    """Pure-Python TCP probe - no netstat, no platform-specific tooling."""
    import socket
    sk = socket.socket()
    sk.settimeout(1.0)
    try:
        sk.connect(("127.0.0.1", port))
        return True
    except OSError:
        return False
    finally:
        sk.close()


def pg_running(data_dir):
    """Ask postmaster.pid whether its PID is still alive, portably."""
    pidf = os.path.join(data_dir, "postmaster.pid")
    if not os.path.exists(pidf):
        return False, pidf
    try:
        with open(pidf, encoding="utf-8", errors="ignore") as fh:
            first = fh.readline().strip()
        if not first.isdigit():
            return False, pidf
        try:
            os.kill(int(first), 0)  # signal 0 = existence check
            return True, pidf
        except (OSError, ValueError):
            return False, pidf
    except Exception:  # noqa: BLE001
        return False, pidf


def clear_stale_pid(data_dir, log):
    """Remove postmaster.pid only when its PID is provably dead."""
    alive, pidf = pg_running(data_dir)
    if os.path.exists(pidf) and not alive:
        try:
            os.remove(pidf)
            log("  removed stale postmaster.pid")
        except Exception as exc:  # noqa: BLE001
            log("  could not remove postmaster.pid: %r" % (exc,))


pg = srv = vite = None
srv_log = open(os.path.join(SETUP, "_run_server.log"), "wb")
vite_log = open(os.path.join(SETUP, "_run_vite.log"), "wb")
pg_logfh = open(PG_LOG, "ab")

try:
    w("=" * 66)
    w(" IIP LOCAL RUN   " + time.strftime("%Y-%m-%d %H:%M:%S"))
    w("=" * 66)
    w()

    # ------------------------------------------------------- 1. PostgreSQL
    w("### [1] PostgreSQL :%d" % PG_PORT)
    PG_DATA = os.path.join(PG_HOME, "data")
    reuse_pg = port_listening(PG_PORT)
    if reuse_pg:
        w("  port %d already in use -> reusing the running instance" % PG_PORT)
    else:
        clear_stale_pid(PG_DATA, w)

    if not reuse_pg:
        pg = subprocess.Popen(
            [POSTGRES, "-D", PG_DATA, "-p", str(PG_PORT)],
            stdin=subprocess.DEVNULL, stdout=pg_logfh, stderr=pg_logfh,
            creationflags=DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW,
            close_fds=True,
        )
        w("  pid=%d  booting (crash recovery may take up to 90s)..." % pg.pid)
        ok = False
        for i in range(1, 181):
            time.sleep(0.5)
            rc, _ = run([PG_ISREADY, "-h", "127.0.0.1", "-p", str(PG_PORT)], timeout=10)
            if rc == 0:
                ok = True
                w("  READY after %.1fs" % (i * 0.5))
                break
            if pg.poll() is not None:
                w("  FATAL pg exited rc=%s  (see %s)" % (pg.returncode, PG_LOG))
                break
        if not ok:
            w("  FATAL: PostgreSQL not ready")
            raise SystemExit(1)
    else:
        w("  READY (pre-existing instance)")

    env = dict(os.environ, PGPASSWORD=PG_PASSWORD, IIP_PG_URL=PGURL)
    rc, out = run([PSQL, "-h", "127.0.0.1", "-p", str(PG_PORT), "-U", PG_USER,
                   "-d", "postgres", "-t", "-A", "-c",
                   "SELECT count(*) FROM pg_database WHERE datname='%s';" % PG_DB],
                  env=env)
    if out.strip() != "1":
        run([PSQL, "-h", "127.0.0.1", "-p", str(PG_PORT), "-U", PG_USER,
             "-d", "postgres", "-q", "-c",
             "CREATE DATABASE %s ENCODING 'UTF8';" % PG_DB],
            env=env)
        w("  created database %s" % PG_DB)
    rc, out = run([PSQL, "-h", "127.0.0.1", "-p", str(PG_PORT), "-U", PG_USER,
                   "-d", PG_DB, "-t", "-A", "-c",
                   "SELECT version||' dirty='||dirty FROM schema_migrations;"], env=env)
    w("  schema_migrations: %s" % out.strip())
    w()

    # ------------------------------------------------------- 2. iip-server
    w("### [2] iip-server :%d" % HTTP_PORT)
    if port_listening(HTTP_PORT) and STRICT_PORT:
        w("  FATAL: port %d already in use - refuse to start a second instance"
          % HTTP_PORT)
        w("  hint: stop the existing server, or set IIP_STRICT_PORT=0 to let it"
          " bind whatever is free")
        raise SystemExit(1)
    if not os.path.exists(SERVER_EXE):
        w("  building...")
        rc, out = run([r"C:\Program Files\Go\bin\go.exe", "build",
                       "-o", SERVER_EXE, "./cmd/iip-server"],
                      timeout=300, cwd=PROJ, env=env)
        w("  build rc=%d" % rc)
    srv = subprocess.Popen(
        [SERVER_EXE, "--http-addr", ":%d" % HTTP_PORT],
        stdin=subprocess.DEVNULL, stdout=srv_log, stderr=srv_log,
        cwd=PROJ, env=env,
        creationflags=CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW,
    )
    w("  pid=%d launching..." % srv.pid)
    ok = False
    for i in range(1, 81):
        time.sleep(0.5)
        st, body = http_get("http://127.0.0.1:%d/healthz" % HTTP_PORT)
        if st == 200:
            ok = True
            w("  READY after %.1fs   GET /healthz -> %d %r" % (i * 0.5, st, body))
            break
        if srv.poll() is not None:
            w("  FATAL server exited rc=%s" % srv.returncode)
            break
    if not ok:
        w("  FATAL: iip-server not healthy")
        raise SystemExit(1)
    w()

    # ------------------------------------------------------- 3. Vite
    w("### [3] web (vite) :%d" % VITE_PORT)
    if port_listening(VITE_PORT) and STRICT_PORT:
        w("  FATAL: port %d already in use - refuse to start a second dev server"
          % VITE_PORT)
        w("  hint: vite would silently fall back to :%d and this script would"
          " then verify the WRONG port" % (VITE_PORT + 1))
        raise SystemExit(1)
    # --strictPort makes vite fail instead of quietly shifting to :PORT+1 when
    # the port is taken. Without it the readiness probe below can pass against
    # a DIFFERENT server that happens to occupy VITE_PORT.
    vite = subprocess.Popen(
        ["pnpm", "dev", "--host", "127.0.0.1", "--port", str(VITE_PORT),
         "--strictPort"],
        stdin=subprocess.DEVNULL, stdout=vite_log, stderr=vite_log,
        cwd=os.path.join(PROJ, "web"), env=env,
        shell=IS_WIN,
        creationflags=CREATE_NEW_PROCESS_GROUP | CREATE_NO_WINDOW,
    )
    w("  pid=%d launching..." % vite.pid)
    ok = False
    for i in range(1, 121):
        time.sleep(0.5)
        st, body = http_get("http://127.0.0.1:%d/" % VITE_PORT)
        if st == 200 and "vite" in body.lower():
            ok = True
            w("  READY after %.1fs   GET / -> %d (%d bytes, vite markers present)"
              % (i * 0.5, st, len(body)))
            break
        if vite.poll() is not None:
            w("  vite exited rc=%s (see _run_vite.log)" % vite.returncode)
            break
    if not ok:
        w("  FATAL: vite not serving on :%d (a different process may hold it)"
          % VITE_PORT)
        raise SystemExit(1)
    w("  vite healthy=True on :%d" % VITE_PORT)

    # ------------------------------------------------------- 4. summary
    w()
    w("=" * 66)
    w(" STACK UP")
    w("=" * 66)
    w("  PostgreSQL  127.0.0.1:%d  pid=%s%s"
      % (PG_PORT, pg.pid if pg else "external", "  (reused)" if reuse_pg else ""))
    w("  iip-server  127.0.0.1:%d  pid=%s" % (HTTP_PORT, srv.pid))
    w("  web (vite)  127.0.0.1:%d  pid=%s" % (VITE_PORT, vite.pid if vite else -1))
    w()
    w("  URLs:")
    w("    http://127.0.0.1:%d/          <- 前端控制台" % VITE_PORT)
    w("    http://127.0.0.1:%d/healthz   <- 健康检查" % HTTP_PORT)
    w()
    w("  logs:")
    w("    %s" % os.path.join(SETUP, "_run_server.log"))
    w("    %s" % os.path.join(SETUP, "_run_vite.log"))
    w("    %s" % PG_LOG)
    w()
    w("  keep-alive: heartbeating every 30s (script must stay alive)")
    w()

    # ------------------------------------------------------- 5. keep alive
    n = 0
    while True:
        time.sleep(30)
        n += 1
        pg_ok = run([PG_ISREADY, "-h", "127.0.0.1", "-p", str(PG_PORT)],
                    timeout=10)[0] == 0
        s_t, s_b = http_get("http://127.0.0.1:%d/healthz" % HTTP_PORT)
        v_t, _ = http_get("http://127.0.0.1:%d/" % VITE_PORT)
        line = ("[%s] beat=%d  pg=%s  server=%s(%s)  vite=%s"
                % (time.strftime("%H:%M:%S"), n, pg_ok, s_t, s_b, v_t))
        w(line)
        if not pg_ok or s_t != 200:
            for p, nm in [(srv, "server"), (pg, "postgres")]:
                if p and p.poll() is not None:
                    w("  !! %s died rc=%s" % (nm, p.returncode))

finally:
    w()
    w("### teardown %s" % time.strftime("%H:%M:%S"))
    # Only stop processes THIS script started. A reused PostgreSQL is left alone.
    for p, nm in [(vite, "vite"), (srv, "iip-server"), (pg, "postgres")]:
        if p is None:
            continue
        if p.poll() is not None:
            w("  %s already exited rc=%s" % (nm, p.returncode))
            continue
        try:
            p.terminate()
            p.wait(timeout=10)
            w("  stopped %s" % nm)
        except Exception:  # noqa: BLE001
            try:
                p.kill()
                w("  killed %s" % nm)
            except Exception:  # noqa: BLE001
                pass
    try:
        open(STATE, "w", encoding="utf-8").write("\n".join(L))
    except Exception:  # noqa: BLE001
        pass
