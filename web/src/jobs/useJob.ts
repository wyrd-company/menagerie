import { useCallback, useEffect, useReducer, useState } from "react";
import { ApiError, getJob, redirectToLogin } from "../api/client.ts";
import type { InterviewReply } from "../api/types.ts";
import { decodeEvent, initialJobView, isTerminal, jobReducer, type JobView } from "./jobView.ts";

const eventTypes = ["state", "log", "interview", "result"] as const;

export interface JobHandle {
  view: JobView;
  /** Applies an InterviewReply that an answers request returned. */
  applyReply: (reply: InterviewReply) => void;
  /** Opens the stream again after it was lost, replaying after the last event seen. */
  reconnect: () => void;
}

/**
 * Follows one job: its summary and its event stream. The stream replays the
 * job's history, so a reload or reconnect rebuilds the same view. The service
 * closes the stream after the result event; the browser then stops listening.
 */
export function useJob(jobId: string | null): JobHandle {
  // Callers key the component by job id, so one hook instance follows one job.
  const [view, dispatch] = useReducer(jobReducer, initialJobView);
  const [attempt, setAttempt] = useState(0);

  useEffect(() => {
    if (jobId === null) return;
    let cancelled = false;
    getJob(jobId)
      .then((job) => {
        if (!cancelled) dispatch({ type: "summary", job });
      })
      .catch((error: unknown) => {
        if (cancelled) return;
        if (error instanceof ApiError && error.status === 404) {
          dispatch({ type: "connection", connection: "gone" });
        }
      });
    return () => {
      cancelled = true;
    };
  }, [jobId, attempt]);

  useEffect(() => {
    if (jobId === null) return;
    const after = view.lastEventId;
    const url = `/api/jobs/${encodeURIComponent(jobId)}/events${after > 0 ? `?lastEventId=${after}` : ""}`;
    const source = new EventSource(url);
    dispatch({ type: "connection", connection: "connecting" });
    let finished = false;

    const onEvent = (type: string) => (message: MessageEvent<string>) => {
      const event = decodeEvent(type, message.lastEventId, message.data);
      dispatch({ type: "event", event });
      if (type === "result") {
        finished = true;
        source.close();
        dispatch({ type: "connection", connection: "closed" });
      }
    };
    const handlers = eventTypes.map((type) => [type, onEvent(type)] as const);
    for (const [type, handler] of handlers) source.addEventListener(type, handler);
    const onUnnamed = onEvent("message");
    source.addEventListener("message", onUnnamed);
    source.onopen = () => dispatch({ type: "connection", connection: "open" });
    source.onerror = () => {
      if (finished || source.readyState !== EventSource.CLOSED) return;
      // The browser gave up: find out whether the job still exists.
      getJob(jobId)
        .then((job) => {
          dispatch({ type: "summary", job });
          dispatch({ type: "connection", connection: "lost" });
        })
        .catch((error: unknown) => {
          if (error instanceof ApiError && error.status === 404) {
            dispatch({ type: "connection", connection: "gone" });
          } else if (error instanceof ApiError && error.status === 401) {
            redirectToLogin();
          } else {
            dispatch({ type: "connection", connection: "lost" });
          }
        });
    };
    return () => {
      source.close();
      for (const [type, handler] of handlers) source.removeEventListener(type, handler);
      source.removeEventListener("message", onUnnamed);
    };
    // Reconnects are explicit: `attempt` reopens from the last event seen.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [jobId, attempt]);

  const applyReply = useCallback(
    (reply: InterviewReply) => dispatch({ type: "interview", reply }),
    [],
  );
  const reconnect = useCallback(() => setAttempt((n) => n + 1), []);

  return { view, applyReply, reconnect };
}

export { isTerminal };
