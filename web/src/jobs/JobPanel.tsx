import { useEffect, useRef, useState } from "react";
import { ApiError, answerJob } from "../api/client.ts";
import type { EndedDocument, Submission } from "../api/types.ts";
import { InterviewForm } from "../interview/InterviewForm.tsx";
import { Messages } from "../interview/Messages.tsx";
import { JobConsole } from "./JobConsole.tsx";
import { isTerminal } from "./jobView.ts";
import { useJob } from "./useJob.ts";

interface Props {
  jobId: string;
  heading?: string;
  onFinished?: () => void;
}

/** One job: its interview while it asks questions, then its live console. */
export function JobPanel({ jobId, heading, onFinished }: Props) {
  const { view, applyReply, reconnect } = useJob(jobId);
  const [submitting, setSubmitting] = useState(false);
  const [submitError, setSubmitError] = useState<string | null>(null);
  // Set before the request starts, so a second keypress in the same frame cannot send again.
  const inFlight = useRef(false);
  const terminal = isTerminal(view);

  useEffect(() => {
    if (terminal) onFinished?.();
    // Only the transition matters; onFinished may change identity each render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [terminal]);

  const reply = view.interview;
  const asking =
    reply !== null &&
    reply.state === "questions" &&
    reply.document?.status === "questions" &&
    !terminal;

  const submit = async (submission: Submission) => {
    if (!reply || inFlight.current) return;
    inFlight.current = true;
    setSubmitting(true);
    setSubmitError(null);
    try {
      const next = await answerJob(jobId, reply.session, submission);
      if (next) applyReply(next);
    } catch (e) {
      setSubmitError(e instanceof ApiError ? `${e.kind}: ${e.message}` : String(e));
    } finally {
      inFlight.current = false;
      setSubmitting(false);
    }
  };

  return (
    <div className="job-panel">
      {asking && reply.document?.status === "questions" ? (
        <InterviewForm
          document={reply.document}
          documentText={reply.documentText}
          ordinal={view.batch}
          submitting={submitting}
          submitError={submitError}
          onSubmit={(s) => void submit(s)}
        />
      ) : null}
      {reply?.state === "completed" && !terminal ? (
        <p className="notice notice-good" role="status">
          The interview is complete. Menagerie is writing the workspace.
        </p>
      ) : null}
      {reply?.document?.status === "ended" ? <EndedNotice document={reply.document} /> : null}
      {reply && reply.warnings.length > 0 ? (
        <Messages
          messages={reply.warnings.map((w) => (w.startsWith("warning: ") ? w : `warning: ${w}`))}
        />
      ) : null}
      <JobConsole view={view} onReconnect={reconnect} heading={heading} />
    </div>
  );
}

function EndedNotice({ document }: { document: EndedDocument }) {
  return (
    <div className="notice notice-warn" role="status">
      <p>
        The template ended the interview with <strong className="mono">{document.kind}</strong>
        {document.label ? (
          <>
            {" "}
            (<span className="mono">{document.label}</span>)
          </>
        ) : null}
        .
      </p>
      <Messages messages={document.messages} />
    </div>
  );
}
