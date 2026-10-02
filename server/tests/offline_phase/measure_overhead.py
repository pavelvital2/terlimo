"""Offline overhead fixture: fake awaits, real counters and standard logging sink."""
import asyncio
import json
import logging
import os
import statistics
import tempfile
import time
from terlimo_backend.auth_api_phase import FLAG, new_auth_api_phase, logger

async def sample(enabled):
    os.environ[FLAG] = "1" if enabled else "0"
    wall = time.perf_counter_ns();cpu = time.thread_time_ns()
    phase = new_auth_api_phase("session")
    if phase:phase.activate("a" * 32)
    for _ in range(10):
        if phase:phase.mark("fake_await_begin")
        await asyncio.sleep(0)
        if phase:phase.mark("fake_await_end")
    if phase:phase.finish()
    return time.perf_counter_ns()-wall, time.thread_time_ns()-cpu

async def main():
    result={};logger.setLevel(logging.INFO);logger.propagate=False
    for mode in ["filtered_existing_sink", "standard_stream_sink"]:
        with tempfile.TemporaryFile(mode="w+") as stream:
            handler=logging.StreamHandler(stream);handler.setLevel(logging.INFO if mode == "standard_stream_sink" else logging.WARNING)
            logger.handlers=[handler]
            for i in range(30):await sample(bool(i % 2))
            values={False:[],True:[]}
            for i in range(200):
                for enabled in ([False,True] if i%2 else [True,False]):values[enabled].append(await sample(enabled))
            def stats(vals):
                return {"wall_median_ns":int(statistics.median(v[0] for v in vals)),"wall_p95_ns":sorted(v[0] for v in vals)[int(len(vals)*.95)-1],"thread_cpu_median_ns":int(statistics.median(v[1] for v in vals))}
            result[mode]={"off":stats(values[False]),"on":stats(values[True]),"samples_per_mode":200,"phase_records_per_on_request":20,"writes_per_on_request":1 if mode == "standard_stream_sink" else 0,"log_bytes_including_warmup":stream.tell()}
            result[mode]["median_wall_delta_ns"]=result[mode]["on"]["wall_median_ns"]-result[mode]["off"]["wall_median_ns"]
    result["limits"]="Local synthetic coroutine; real proc/thread counters; temporary file StreamHandler flush, no fsync/journald. Shared thread/host. Not production overhead, historical attribution, or zero overhead."
    print(json.dumps(result,indent=2))

if __name__ == "__main__":asyncio.run(main())
