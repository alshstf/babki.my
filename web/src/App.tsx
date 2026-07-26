import { useEffect, useState } from "react";

type Health = { status: string; version: string };

export default function App() {
  const [health, setHealth] = useState<Health | null>(null);
  const [error, setError] = useState(false);

  useEffect(() => {
    fetch("/api/healthz")
      .then((r) => r.json())
      .then(setHealth)
      .catch(() => setError(true));
  }, []);

  return (
    <div className="min-h-screen bg-neutral-950 text-neutral-100 flex flex-col items-center justify-center gap-4">
      <h1 className="text-4xl font-bold tracking-tight">babki.my</h1>
      <p className="text-neutral-400">Учёт семейных финансов. Скоро здесь будет дашборд.</p>
      <div className="text-sm text-neutral-500">
        {error && <span>сервер недоступен</span>}
        {health && (
          <span>
            сервер: {health.status} · версия {health.version}
          </span>
        )}
        {!health && !error && <span>проверяю сервер…</span>}
      </div>
    </div>
  );
}
