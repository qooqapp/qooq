export const metadata = { title: "nextjs-app" };

export default function RootLayout({ children }) {
  return (
    <html lang="en">
      <body style={{ fontFamily: "system-ui, sans-serif", margin: "3rem" }}>{children}</body>
    </html>
  );
}
