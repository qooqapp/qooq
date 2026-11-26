import { useState } from "react";

export default function App() {
  const [message, setMessage] = useState("(not fetched yet)");

  async function fetchHello() {
    const res = await fetch("/api/hello");
    const data = await res.json();
    setMessage(data.message);
  }

  return (
    <div style={{ fontFamily: "system-ui", maxWidth: "40rem", margin: "4rem auto" }}>
      <h1>qooq fullstack-server</h1>
      <button onClick={fetchHello}>Call /api/hello</button>
      <p>{message}</p>
    </div>
  );
}
