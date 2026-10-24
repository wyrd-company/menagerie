const warningPrefix = "warning: ";

/** Toha messages in interview order; a Toha warning starts with `warning: `. */
export function Messages({ messages }: { messages: string[] }) {
  if (messages.length === 0) return null;
  return (
    <ul className="messages" aria-label="Messages from the template">
      {messages.map((message, index) => {
        const warning = message.startsWith(warningPrefix);
        return (
          <li key={index} className={warning ? "message warning" : "message"}>
            {warning ? <strong>Warning</strong> : null}
            <span>{warning ? message.slice(warningPrefix.length) : message}</span>
          </li>
        );
      })}
    </ul>
  );
}
