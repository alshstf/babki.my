import { localToday } from "@/lib/dates";

// How close a card's payment day must be for the card's page and the
// reminders to speak up.
export const SOON_DAYS = 7;

// daysUntil is how many days from today to an ISO date; negative once past.
export function daysUntil(iso: string): number {
  return Math.round((Date.parse(iso) - Date.parse(localToday())) / 86_400_000);
}
