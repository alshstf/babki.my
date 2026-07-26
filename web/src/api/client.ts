import createClient from "openapi-fetch";
import type { paths } from "./schema";

// Single typed API client. Cookies flow automatically (same-origin).
export const api = createClient<paths>({ baseUrl: "/" });
