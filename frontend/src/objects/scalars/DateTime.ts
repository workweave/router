declare const dateTimeBrand: unique symbol;

/**
 * A date and time, represented as a number of milliseconds since the Unix epoch.
 */
export type DateTime = number & { readonly [dateTimeBrand]: "DateTimeScalar" };

export const DateTime = {
  toDate: (dateTime: DateTime): Date => new Date(dateTime),
  fromDate: (date: Date): DateTime => date.getTime() as DateTime,
  now: (): DateTime => Date.now() as DateTime,
};
