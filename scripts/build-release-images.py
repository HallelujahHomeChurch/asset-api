"""Build the fixed release images with at most two Docker clients in flight."""
import os
import signal
import subprocess
import sys
import time

BUILDS = [('api', 'Dockerfile'), ('scan', 'Dockerfile.scan'), ('recording', 'Dockerfile.recording')]


def stop(processes):
    groups = {process.pid: process for process, _, _ in processes}
    for pid in list(groups):
        try:
            os.killpg(pid, signal.SIGTERM)
        except ProcessLookupError:
            groups.pop(pid)
        except PermissionError:
            pass
    deadline = time.monotonic() + 5
    while groups and time.monotonic() < deadline:
        for pid, process in list(groups.items()):
            process.poll()
            try:
                os.killpg(pid, 0)
            except ProcessLookupError:
                groups.pop(pid)
            except PermissionError:
                # Retry without treating a permission failure as proof that the group exited.
                pass
        if groups:
            time.sleep(0.05)
    failed = False
    for pid in groups:
        try:
            os.killpg(pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        except PermissionError:
            failed = True
    for process, _, _ in processes:
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            failed = True
    if failed:
        raise RuntimeError('could not stop all owned image builds')


def main():
    interrupted = 0

    def interrupt(number, _frame):
        nonlocal interrupted
        interrupted = number

    signal.signal(signal.SIGINT, interrupt)
    signal.signal(signal.SIGTERM, interrupt)
    active = []
    pending = list(BUILDS)
    started = time.monotonic()
    try:
        while active or pending:
            if interrupted:
                return 128 + interrupted
            for process, role, began in list(active):
                result = process.poll()
                if result is None:
                    continue
                if result:
                    print(f'image_build_failed role={role} exit={result}', file=sys.stderr, flush=True)
                    return result if result > 0 else 128 - result
                active.remove((process, role, began))
                print(f'image_build_completed role={role} elapsed_ms={int((time.monotonic()-began)*1000)}', flush=True)
            while pending and len(active) < 2 and not interrupted:
                role, dockerfile = pending.pop(0)
                print(f'image_build_started role={role}', flush=True)
                process = subprocess.Popen(['docker', 'build', '-f', dockerfile, '-t', f'asset-{role}:verify', '.'], start_new_session=True)
                active.append((process, role, time.monotonic()))
            if active:
                time.sleep(0.05)
        print(f'image_builds_completed=3 elapsed_ms={int((time.monotonic()-started)*1000)}', flush=True)
        return 0
    finally:
        stop(active)


if __name__ == '__main__':
    sys.exit(main())
