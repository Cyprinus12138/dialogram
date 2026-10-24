---
name: trip-planner
description: Plan a short trip that combines the five-day forecast with a flight. Use when the user asks for a trip plan, an itinerary, or which day to travel, rather than for a single weather or flight lookup.
metadata:
  author: dialogram-examples
  version: "1.0"
---

# Trip planner

1. Make sure you know the origin city, the destination and the travel date.
   Ask for whatever is missing with request_user_form in one go.
2. Call get_weather for the destination. Note the dry days and the days with
   a high rain chance.
3. Call search_flights for the travel date. Show the options with
   render_from_ref as a comparison_table, not as retyped rows.
4. Suggest which days suit outdoor plans, quoting only the figures the
   forecast returned. For what to pack, follow references/packing.md.
5. When the user picks a flight, call book_flight. The user approves the
   booking before it runs.
