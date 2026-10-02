import { useEffect, useState } from "react";

type Hello = { message: string; pid: number; started: string };

export default function App() {
  const [hello, setHello] = useState<Hello | null>(null);
  const [error, setError] = useState<string | null>(null);

  async function load() {
    try {
      const res = await fetch("/api/hello");
      if (!res.ok) throw new Error(`${res.status} ${await res.text()}`);
      setHello(await res.json());
      setError(null);
    } catch (e) {
      setError(String(e));
    }
  }

  useEffect(() => {
    load();
  }, []);

  return (
    <main>
      <h1>⚡ flashpoint</h1>
      <p className="lede">
        Edit <code>main.go</code> for the API or <code>src/App.tsx</code> for this page.
      </p>
      {hello && (
        <section>
          <p className="message">{hello.message}</p>
          <p className="meta">
            served by pid {hello.pid}, up since {new Date(hello.started).toLocaleTimeString()}
          </p>
        </section>
      )}
      {error && <pre className="error">{error}</pre>}
      <button onClick={load}>Ask the API again</button>
    </main>
  );
}
