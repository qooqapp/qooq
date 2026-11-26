import { StrictMode, useState } from "react";
import { createRoot } from "react-dom/client";
import { BrowserRouter, Link, Route, Routes } from "react-router-dom";

function Counter() {
  const [n, setN] = useState(0);
  return (
    <>
      <h1>Hello from Vite + React</h1>
      <button onClick={() => setN(n + 1)}>clicked {n} times</button>
      <p><Link to="/about">About</Link> (a client-side route: reloading it still works)</p>
    </>
  );
}

function About() {
  return (
    <>
      <h1>About</h1>
      <p><Link to="/">Back</Link></p>
    </>
  );
}

createRoot(document.getElementById("root")).render(
  <StrictMode>
    <BrowserRouter>
      <Routes>
        <Route path="/" element={<Counter />} />
        <Route path="/about" element={<About />} />
      </Routes>
    </BrowserRouter>
  </StrictMode>
);
