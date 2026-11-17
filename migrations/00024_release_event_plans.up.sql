-- Keep the old code for existing purchase and payment references.
UPDATE event_plans SET active = false WHERE code = 'standard';

INSERT INTO event_plans (code, name, price, capacity_stands) VALUES
    ('mini', 'Mini', 180000, 20),
    ('basic', 'Basic', 250000, 30),
    ('standart', 'Standart', 300000, 40),
    ('plus', 'Plus', 350000, 50),
    ('medium', 'Medium', 475000, 75),
    ('large', 'Large', 600000, 100),
    ('custom', 'Custom', 600000, NULL);
